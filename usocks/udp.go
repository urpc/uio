/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package usocks

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio"
)

var errTooManyAssociations = errors.New("usocks: too many UDP associations")

// Bounds on the UDP relay's off-turn work and per-association state, so a single
// client cannot exhaust goroutines or memory.
const (
	maxUDPResolvers   = 64   // concurrent domain-target resolutions
	maxUDPTargets     = 4096 // whitelisted reply sources per association
	udpResolveTimeout = 5 * time.Second
)

// udpRelay runs the UDP ASSOCIATE data plane (RFC 1928 §6-7): one shared UDP
// Events hosting a relay listener per association. A listener's socket receives
// both the client's SOCKS-framed datagrams and the targets' raw replies as
// per-source peers; the Handler routes by address. Each association owns its
// socket, so a target's reply is unambiguous.
type udpRelay struct {
	ev     *uio.Events
	opts   Options
	dnsSem chan struct{}  // bounds concurrent domain-target resolutions
	dnsWG  sync.WaitGroup // tracks in-flight resolver goroutines for wait

	mu        sync.Mutex
	assocs    map[netip.AddrPort]*udpAssociation // relay listener addr -> association
	closed    bool
	stop      chan struct{}
	sweepDone chan struct{} // closed when the idle sweep goroutine exits
}

// udpAssociation is one UDP ASSOCIATE: its relay listener, the TCP control
// channel that bounds its life, and the locked client UDP address. Replies are
// accepted only from targets the client has contacted (targets), so a foreign
// source cannot inject forged replies; the client address is pinned to the TCP
// peer's IP (and the request's advertised address when given), so a foreign
// source cannot hijack the association.
type udpAssociation struct {
	relay      *udpRelay
	clientTCP  uio.Conn
	ln         uio.PacketListener
	addr       netip.AddrPort // relay listener address (the table key)
	clientIP   netip.Addr     // the TCP control peer's IP, for pinning
	expectAddr netip.AddrPort // the address the request advertised (zero if none)
	username   string         // authenticated username, for per-datagram Policy
	clientAddr netip.AddrPort
	clientSet  bool
	lastActive atomic.Int64
	closed     atomic.Bool
	frag       fragReassembly

	ctx    context.Context
	cancel context.CancelFunc

	tmu     sync.Mutex
	targets map[netip.AddrPort]struct{} // reply sources the client has contacted
}

// fragReassembly buffers one fragment sequence (RFC 1928 §7). It is touched
// only on the association's relay turns. The queue is abandoned on timeout or
// when a fragment arrives whose position regresses below the highest seen.
type fragReassembly struct {
	frags    map[byte][]byte
	maxSeq   byte
	lastPos  byte
	hasLast  bool
	total    int
	deadline time.Time
}

func (f *fragReassembly) reset() {
	f.frags = nil
	f.maxSeq = 0
	f.hasLast = false
	f.lastPos = 0
	f.total = 0
}

func newUDPRelay(opts Options) *udpRelay {
	r := &udpRelay{
		opts:      opts,
		assocs:    make(map[netip.AddrPort]*udpAssociation),
		stop:      make(chan struct{}),
		sweepDone: make(chan struct{}),
		dnsSem:    make(chan struct{}, maxUDPResolvers),
	}
	// A SOCKS UDP datagram can carry a full 65535-byte UDP payload; the default
	// 4 KiB read buffer would truncate it. Read whole datagrams.
	r.ev = &uio.Events{Handler: r, ReadBufferSize: 64 << 10}
	go r.ev.Serve()
	go r.sweep()
	return r
}

// associate allocates a relay listener for a UDP ASSOCIATE and returns it with
// the bound address to advertise to the client (the TCP local IP + relay port).
// expect is the client address the request advertised (zero if 0.0.0.0:0);
// username is the authenticated user, carried to per-datagram Policy.
func (r *udpRelay) associate(clientTCP uio.Conn, expect netip.AddrPort, username string) (*udpAssociation, netip.AddrPort, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, netip.AddrPort{}, net.ErrClosed
	}
	if r.opts.MaxUDPAssociations > 0 && len(r.assocs) >= r.opts.MaxUDPAssociations {
		r.mu.Unlock()
		return nil, netip.AddrPort{}, errTooManyAssociations
	}
	r.mu.Unlock()

	ln, err := r.listen()
	if err != nil {
		return nil, netip.AddrPort{}, err
	}
	addr := addrPortOf(ln.Addr())
	ctx, cancel := context.WithCancel(context.Background())
	a := &udpAssociation{
		relay:      r,
		clientTCP:  clientTCP,
		ln:         ln,
		addr:       addr,
		clientIP:   addrPortOf(clientTCP.RemoteAddr()).Addr().Unmap(),
		expectAddr: netip.AddrPortFrom(expect.Addr().Unmap(), expect.Port()),
		username:   username,
		ctx:        ctx,
		cancel:     cancel,
		targets:    make(map[netip.AddrPort]struct{}),
	}
	a.lastActive.Store(time.Now().UnixNano())
	r.mu.Lock()
	// Recheck under the lock: concurrent associates may have filled the cap or
	// the relay may have closed while this one was listening.
	if r.closed || (r.opts.MaxUDPAssociations > 0 && len(r.assocs) >= r.opts.MaxUDPAssociations) {
		r.mu.Unlock()
		cancel()
		_ = ln.Close()
		return nil, netip.AddrPort{}, errTooManyAssociations
	}
	r.assocs[addr] = a
	r.mu.Unlock()
	return a, boundAddr(clientTCP, addr.Port()), nil
}

func (r *udpRelay) listen() (uio.PacketListener, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		ln, err := r.ev.Listen("udp://:0")
		if err == nil {
			return ln.(uio.PacketListener), nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(time.Millisecond)
	}
}

// close ends the association: it releases the relay socket, cancels in-flight
// resolutions, and closes the TCP control channel so an idle association does
// not leak the control connection's FD or Limiter slot.
func (a *udpAssociation) close() {
	if a.closed.CompareAndSwap(false, true) {
		a.relay.mu.Lock()
		delete(a.relay.assocs, a.addr)
		a.relay.mu.Unlock()
		a.cancel()
		_ = a.ln.Close()
		_ = a.clientTCP.Close()
	}
}

func (r *udpRelay) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.stop)
	assocs := make([]*udpAssociation, 0, len(r.assocs))
	for _, a := range r.assocs {
		assocs = append(assocs, a)
	}
	r.assocs = make(map[netip.AddrPort]*udpAssociation)
	r.mu.Unlock()
	for _, a := range assocs {
		a.cancel()
		_ = a.ln.Close()
	}
	_ = r.ev.Close(nil)
}

// wait blocks until the relay's loops, idle sweep and in-flight resolver
// goroutines have stopped. It is called after close, so the sweep's stop channel
// is already closed and each association's context is cancelled.
func (r *udpRelay) wait() {
	_ = r.ev.Wait()
	<-r.sweepDone
	r.dnsWG.Wait()
}

// sweep closes associations idle past the timeout, so a forgotten client (or a
// TCP control channel that died without OnClose) does not leak a relay socket.
func (r *udpRelay) sweep() {
	defer close(r.sweepDone)
	interval := r.opts.udpIdleTimeout() / 2
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return
		}
		now := time.Now().UnixNano()
		var idle []*udpAssociation
		for _, a := range r.assocs {
			if now-a.lastActive.Load() > int64(r.opts.udpIdleTimeout()) {
				idle = append(idle, a)
			}
		}
		r.mu.Unlock()
		for _, a := range idle {
			a.close()
		}
	}
}

// OnRead routes one datagram: a client's SOCKS-framed request goes to its target
// (header stripped); a reply goes back to the client (header added) only if it
// comes from a target the client contacted. A foreign source is dropped.
func (r *udpRelay) OnRead(peer uio.Conn) error {
	local := addrPortOf(peer.LocalAddr())
	r.mu.Lock()
	a := r.assocs[local]
	r.mu.Unlock()
	if a == nil || a.closed.Load() {
		return nil
	}
	data := peer.PeekChunk()
	if len(data) == 0 {
		return nil
	}
	// Normalize to the unmapped form: a dual-stack relay reports IPv4 peers as
	// IPv4-mapped IPv6, while a target parsed from a literal is plain IPv4. One
	// representation keeps the client lock, the source pin and the reply
	// whitelist consistent. WriteTo re-maps as the socket family requires.
	remote := addrPortOf(peer.RemoteAddr())
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())

	if !a.clientSet {
		// Pin the client to the TCP control peer's IP (RFC 1928 §7): a datagram
		// from another IP cannot claim the association. When the request
		// advertised a specific address AND port, require it exactly; a port of
		// 0 means "port unknown", so it falls back to the IP pin above.
		if remote.Addr() != a.clientIP {
			return nil
		}
		if a.expectAddr.IsValid() && !a.expectAddr.Addr().IsUnspecified() && a.expectAddr.Port() != 0 && remote != a.expectAddr {
			return nil
		}
		a.clientAddr = remote
		a.clientSet = true
	}

	if remote == a.clientAddr {
		a.lastActive.Store(time.Now().UnixNano())
		frag, host, port, payload, ok, err := parseUDPHeader(data)
		if err != nil || !ok {
			return nil // malformed: drop silently (RFC 1928 §7)
		}
		out := payload
		if frag != 0 {
			complete, full := r.reassemble(a, frag, payload)
			if !complete {
				return nil
			}
			out = full
		}
		if r.opts.Policy != nil {
			req := Request{Cmd: CmdUDPAssociate, Host: host, Port: port, ClientAddr: a.clientAddr, Username: a.username}
			if r.opts.Policy.Allow(context.Background(), &req) != nil {
				return nil
			}
		}
		r.forwardToTarget(a, host, port, out)
		return nil
	}

	// A reply: accept it only from a target the client has contacted, so a
	// foreign source cannot inject forged replies.
	if !a.hasTarget(remote) {
		return nil
	}
	a.lastActive.Store(time.Now().UnixNano())
	framed := appendUDPHeader(nil, remote.Addr().String(), remote.Port(), data)
	_, _ = a.ln.WriteTo(framed, a.clientAddr)
	return nil
}

func (a *udpAssociation) addTarget(addr netip.AddrPort) {
	key := netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	a.tmu.Lock()
	if len(a.targets) >= maxUDPTargets {
		a.targets = make(map[netip.AddrPort]struct{}) // bound memory; restart the set
	}
	a.targets[key] = struct{}{}
	a.tmu.Unlock()
}

func (a *udpAssociation) hasTarget(addr netip.AddrPort) bool {
	key := netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	a.tmu.Lock()
	_, ok := a.targets[key]
	a.tmu.Unlock()
	return ok
}

// reassemble buffers a fragment (RFC 1928 §7) and reports the completed
// datagram once the last fragment arrives and every position is present. The
// queue is abandoned on timeout or a position regression below the highest
// seen. payload is borrowed, so fragments are copied.
func (r *udpRelay) reassemble(a *udpAssociation, frag byte, payload []byte) (bool, []byte) {
	pos := frag & 0x7F
	if pos == 0 {
		return false, nil // FRAG != 0 with position 0 is invalid
	}
	f := &a.frag
	now := time.Now()
	if now.After(f.deadline) || pos < f.maxSeq {
		f.reset()
	}
	if f.frags == nil {
		f.frags = make(map[byte][]byte)
		f.deadline = now.Add(r.opts.udpReassemblyTimeout())
	}
	cp := append([]byte(nil), payload...)
	// A retransmitted fragment replaces its position: count the distinct bytes,
	// not the sum of every arrival, so duplicates cannot inflate the total past
	// the UDP maximum and drop a legitimate sequence.
	f.total += len(cp) - len(f.frags[pos])
	if f.total > 65535 { // a reassembled datagram cannot exceed the UDP maximum
		f.reset()
		return false, nil
	}
	f.frags[pos] = cp
	f.maxSeq = pos
	if frag&0x80 != 0 {
		f.hasLast = true
		f.lastPos = pos
	}
	if !f.hasLast {
		return false, nil
	}
	for i := byte(1); i <= f.lastPos; i++ {
		if _, ok := f.frags[i]; !ok {
			return false, nil // a position is missing: keep buffering
		}
	}
	var full []byte
	for i := byte(1); i <= f.lastPos; i++ {
		full = append(full, f.frags[i]...)
	}
	f.reset()
	return true, full
}

func (r *udpRelay) forwardToTarget(a *udpAssociation, host string, port uint16, payload []byte) {
	if ip, err := netip.ParseAddr(host); err == nil {
		target := netip.AddrPortFrom(ip, port)
		a.addTarget(target)
		_, _ = a.ln.WriteTo(payload, target)
		return
	}
	// A domain target: resolve off the turn, bounded by a semaphore so a flood
	// of unique domains cannot spawn unbounded goroutines, with a timeout and
	// the association's context so a close cancels in-flight resolutions.
	select {
	case r.dnsSem <- struct{}{}:
	default:
		return // resolver budget exhausted: drop this datagram
	}
	cp := append([]byte(nil), payload...)
	r.dnsWG.Add(1)
	go func() {
		defer r.dnsWG.Done()
		defer func() { <-r.dnsSem }()
		ctx, cancel := context.WithTimeout(a.ctx, udpResolveTimeout)
		defer cancel()
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 || a.closed.Load() {
			return
		}
		target := netip.AddrPortFrom(ips[0], port)
		a.addTarget(target)
		_, _ = a.ln.WriteTo(cp, target)
	}()
}

func (r *udpRelay) OnOpen(uio.Conn)              {}
func (r *udpRelay) OnTimer(uio.Conn) error       { return nil }
func (r *udpRelay) OnClose(uio.Conn, error)      {}
func (r *udpRelay) OnBytesRead(uio.Conn, int)    {}
func (r *udpRelay) OnBytesWritten(uio.Conn, int) {}
func (r *udpRelay) OnStart(*uio.Events)          {}
func (r *udpRelay) OnStop(*uio.Events)           {}

// boundAddr is the address to advertise for the relay: the TCP control channel's
// local IP (reachable by the client) with the relay's port.
func boundAddr(clientTCP uio.Conn, port uint16) netip.AddrPort {
	if ta, ok := clientTCP.LocalAddr().(*net.TCPAddr); ok {
		if ip, ok := netip.AddrFromSlice(ta.IP); ok {
			return netip.AddrPortFrom(ip.Unmap(), port)
		}
	}
	return netip.AddrPortFrom(netip.IPv4Unspecified(), port)
}
