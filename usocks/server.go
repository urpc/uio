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
	"syscall"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ubridge"
)

// handshakeState is the progress of a client leg's SOCKS5 negotiation.
type handshakeState int

const (
	stateMethod handshakeState = iota
	stateAuth
	stateRequest
	stateDone
)

// Server is a SOCKS5 proxy built as a uio Handler wrapping a ubridge.Bridge.
// The connection stays a plain socket for its whole life: the handshake runs as
// an OnRead state machine, and once it completes the server dials the target
// (CONNECT) or allocates a UDP relay (UDP ASSOCIATE), answers, and relays. With
// no layer stack mediating reads, a CONNECT relay uses native splice(2) — the
// zero-copy path a plain tunnel uses.
//
// Governance is pluggable via Options (Authenticator/Policy/Limiter); the relay
// data plane — backpressure, half-close, abort passthrough — is ubridge's.
type Server struct {
	bridge  *ubridge.Bridge
	relay   *udpRelay
	ev      *uio.Events
	opts    Options
	dialSem chan struct{}
	states  sync.Map // client leg Conn -> *connState, during the handshake only

	handshakes   atomic.Uint64
	connects     atomic.Uint64
	dialFailures atomic.Uint64
	authFailures atomic.Uint64
	denied       atomic.Uint64
}

// upstreamMark tags the dialed target leg so the server's callbacks route it
// straight to the bridge instead of the handshake.
type upstreamMark struct{}

// connState is one client leg's handshake progress.
type connState struct {
	state    handshakeState
	buf      []byte
	req      Request
	username string
	release  func()
	target   uio.Conn        // the paired target leg, once the handshake completes
	udp      *udpAssociation // set for a UDP ASSOCIATE control channel
	done     bool
}

// NewServer builds a SOCKS5 server. The zero Options is a no-auth, allow-all,
// unlimited CONNECT proxy with default timeouts.
func NewServer(opts Options) *Server {
	s := &Server{opts: opts}
	if opts.MaxConcurrentDials > 0 {
		s.dialSem = make(chan struct{}, opts.MaxConcurrentDials)
	}
	s.opts.onAuthFailure = func() { s.authFailures.Add(1) }
	s.bridge = &ubridge.Bridge{
		InboundLimit: opts.InboundLimit,
	}
	s.relay = newUDPRelay(opts)
	s.ev = &uio.Events{
		Handler:        s,
		Observer:       s.bridge,
		Hooks:          s.bridge, // bridge.OnStart binds the bridge to this Events
		AllowHalfOpen:  true,
		ReadBufferSize: 64 << 10,
	}
	if opts.InboundLimit > 0 {
		s.ev.MaxInboundBuffered = opts.InboundLimit
	}
	// Couple the relay's life to the main engine's: whenever the engine's
	// lifecycle completes — Close, a Serve that failed to initialize, or a stop
	// through the Events() escape hatch — the relay stops too, so Wait joins it
	// rather than hanging on a relay nothing else will close.
	go func() {
		_ = s.ev.Wait()
		s.relay.close()
	}()
	return s
}

// Serve runs the proxy. With an address it listens there and serves; with no
// address it serves the listeners already added by Listen. It blocks until the
// server stops, returning the cause.
func (s *Server) Serve(addrs ...string) error { return s.ev.Serve(addrs...) }

// Events is the server's underlying Events, an escape hatch for advanced
// configuration the Server does not wrap: tuning Pollers/Executor, adding
// listeners that share the worker pool, or reading transport-level uio.Stats.
// Ordinary serving needs only Serve/Listen/Close/Wait.
func (s *Server) Events() *uio.Events { return s.ev }

// Listen adds a SOCKS5 listener. The connections are plain sockets; the
// handshake runs in the Handler, so no layer is installed.
func (s *Server) Listen(addr string) (uio.Listener, error) { return s.ev.Listen(addr) }

// Close initiates shutdown: it stops accepting and ends the live relays,
// returning without waiting. Use Wait to block until everything has stopped.
func (s *Server) Close() error {
	s.relay.close()
	return s.ev.Close(nil)
}

// Wait blocks until the server has stopped every loop and connection task —
// including the UDP relay's own loops — and OnStop has returned.
func (s *Server) Wait() error {
	err := s.ev.Wait()
	s.relay.wait()
	return err
}

// Stats is a point-in-time, zero-allocation snapshot of the server.
type Stats struct {
	Transport    ubridge.Stats // the relay bridge's snapshot (pairs, legs, bytes)
	Handshakes   uint64        // CONNECT requests that completed the handshake
	Connects     uint64        // relays successfully established
	DialFailures uint64        // target dials that failed (or were refused for capacity)
	AuthFailures uint64        // authentication attempts that failed
	Denied       uint64        // requests refused by the Limiter or Policy
}

// Stats returns a snapshot of s.
func (s *Server) Stats() Stats {
	return Stats{
		Transport:    s.bridge.Stats(),
		Handshakes:   s.handshakes.Load(),
		Connects:     s.connects.Load(),
		DialFailures: s.dialFailures.Load(),
		AuthFailures: s.authFailures.Load(),
		Denied:       s.denied.Load(),
	}
}

// OnOpen initializes a client leg's handshake (the target leg goes straight to
// the bridge) and arms the handshake deadline.
func (s *Server) OnOpen(c uio.Conn) {
	if _, up := c.Userdata().(upstreamMark); up {
		return
	}
	s.states.Store(c, &connState{})
	_ = c.SetWakeDeadline(time.Now().Add(s.opts.handshakeTimeout()))
}

// OnRead runs the handshake state machine on a client leg, then hands the
// connection to the bridge for the relay; a target leg relays directly.
func (s *Server) OnRead(c uio.Conn) error {
	if _, up := c.Userdata().(upstreamMark); up {
		return s.bridge.OnRead(c)
	}
	v, ok := s.states.Load(c)
	if !ok {
		return nil
	}
	st := v.(*connState)
	if st.done {
		if st.udp != nil {
			_, _ = c.Discard(-1) // UDP control channel carries no relay data
			if c.State()&uio.StateReadEnded != 0 {
				_ = c.Close() // peer ended the control channel: terminate the association
			}
			return nil
		}
		return s.bridge.OnRead(c)
	}
	for {
		chunk := c.PeekChunk()
		if len(chunk) == 0 {
			break
		}
		st.buf = append(st.buf, chunk...)
		if _, err := c.Discard(len(chunk)); err != nil {
			return err
		}
	}
	for !st.done {
		n, err := s.step(c, st)
		if err != nil {
			return err
		}
		if n == 0 {
			if len(st.buf) > maxHandshake {
				return errHandshakeTooLarge
			}
			if c.State()&uio.StateReadEnded != 0 {
				_ = c.Close() // peer ended mid-handshake: nothing can complete it
			}
			return nil
		}
		st.buf = st.buf[n:]
	}
	// Handshake complete: relay any payload that arrived with the request to
	// the target, then let the bridge take over the (now splice-eligible) plain
	// socket.
	if len(st.buf) > 0 && st.target != nil {
		_, _ = st.target.Write(st.buf)
	}
	st.buf = nil
	return s.bridge.OnRead(c)
}

// OnTimer cuts a handshake that has not completed in time.
func (s *Server) OnTimer(c uio.Conn) error {
	if v, ok := s.states.Load(c); ok {
		if st := v.(*connState); !st.done {
			_ = c.Close()
		}
	}
	return nil
}

// OnClose releases the Limiter slot and lets the bridge tear down its leg.
func (s *Server) OnClose(c uio.Conn, err error) {
	if v, ok := s.states.LoadAndDelete(c); ok {
		st := v.(*connState)
		if st.release != nil {
			st.release()
		}
		if st.udp != nil {
			st.udp.close()
		}
	}
	s.bridge.OnClose(c, err)
}

func (s *Server) OnBytesRead(c uio.Conn, n int)    { s.bridge.OnBytesRead(c, n) }
func (s *Server) OnBytesWritten(c uio.Conn, n int) { s.bridge.OnBytesWritten(c, n) }
func (s *Server) OnStart(ev *uio.Events)           { s.bridge.OnStart(ev) }
func (s *Server) OnStop(ev *uio.Events)            { s.bridge.OnStop(ev) }

// step advances the handshake by one message, returning the bytes consumed
// (0 while more are needed). On the request it completes the handshake.
func (s *Server) step(c uio.Conn, st *connState) (int, error) {
	switch st.state {
	case stateMethod:
		methods, n, ok, err := parseMethodNeg(st.buf)
		if err != nil || !ok {
			return 0, err
		}
		method := selectMethod(methods, s.opts.Authenticator)
		if _, err := c.Write([]byte{Version5, method}); err != nil {
			return 0, err
		}
		switch method {
		case MethodNoAcceptable:
			_ = c.Close()
			st.done = true
		case MethodUserPass:
			st.state = stateAuth
		default:
			st.state = stateRequest
		}
		return n, nil
	case stateAuth:
		user, pass, n, ok, err := parseAuth(st.buf)
		if err != nil || !ok {
			return 0, err
		}
		if err := s.opts.Authenticator.Authenticate(context.Background(), user, pass); err != nil {
			if s.opts.onAuthFailure != nil {
				s.opts.onAuthFailure()
			}
			_, _ = c.Write([]byte{authVersion, 0x01})
			_ = c.Close()
			st.done = true
			return n, nil
		}
		if _, err := c.Write([]byte{authVersion, 0x00}); err != nil {
			return 0, err
		}
		st.username = user
		st.state = stateRequest
		return n, nil
	case stateRequest:
		req, n, ok, err := parseRequest(st.buf)
		if err != nil || !ok {
			return 0, err
		}
		req.Username = st.username
		req.ClientAddr = addrPortOf(c.RemoteAddr())
		st.req = req
		if req.Cmd != CmdConnect && req.Cmd != CmdUDPAssociate {
			_, _ = c.Write(failureReply(RepCommandNotSupported))
			_ = c.Close()
			st.done = true
			return n, nil
		}
		st.done = true
		_ = c.SetWakeDeadline(time.Time{}) // handshake done: cancel the deadline
		s.completeHandshake(c, st)
		return n, nil
	}
	return 0, nil
}

// completeHandshake admits the request, dials the target, answers, and pairs
// the legs with the bridge. It runs on the client leg's turn.
func (s *Server) completeHandshake(c uio.Conn, st *connState) {
	req := st.req
	s.handshakes.Add(1)
	ctx := context.Background()

	if s.opts.Limiter != nil {
		release, err := s.opts.Limiter.Admit(ctx, &req)
		if err != nil {
			s.denied.Add(1)
			_, _ = c.Write(failureReply(RepNotAllowed))
			_ = c.Close()
			return
		}
		st.release = release
	}
	// Policy gates every command, including UDP ASSOCIATE, so a denied client
	// cannot allocate a relay or reach a target the policy forbids.
	if s.opts.Policy != nil {
		if err := s.opts.Policy.Allow(ctx, &req); err != nil {
			s.denied.Add(1)
			_, _ = c.Write(failureReply(RepNotAllowed))
			_ = c.Close()
			return
		}
	}
	if req.Cmd == CmdUDPAssociate {
		s.setupUDPAssociate(c, st)
		return
	}

	if s.dialSem != nil {
		select {
		case s.dialSem <- struct{}{}:
			defer func() { <-s.dialSem }()
		default:
			s.dialFailures.Add(1)
			_, _ = c.Write(failureReply(RepGeneralFailure))
			_ = c.Close()
			return
		}
	}

	dctx, cancel := context.WithTimeout(ctx, s.opts.dialTimeout())
	defer cancel()
	target, err := s.dialTarget(dctx, req.Target())
	if err != nil {
		s.dialFailures.Add(1)
		_, _ = c.Write(failureReply(repForDialError(err)))
		_ = c.Close()
		return
	}
	_, _ = c.Write(successReply())
	s.connects.Add(1)
	if err := s.bridge.Pair(c, target); err != nil {
		_ = target.Close()
		_ = c.Close()
		return
	}
	st.target = target
}

// dialTarget reaches a CONNECT target: through the custom Dial hook (adopting
// the established connection it returns) or by a direct dial. Either way the
// target leg is handled by this server's Events, tagged for the bridge.
func (s *Server) dialTarget(ctx context.Context, target string) (uio.Conn, error) {
	if s.opts.Dial != nil {
		nc, err := s.opts.Dial(ctx, target)
		if err != nil {
			return nil, err
		}
		c, err := s.ev.AdoptContext(ctx, nc, uio.WithUserdata(upstreamMark{}))
		if err != nil {
			_ = nc.Close()
			return nil, err
		}
		return c, nil
	}
	return s.ev.DialContext(ctx, "tcp://"+target, uio.WithUserdata(upstreamMark{}))
}

// setupUDPAssociate answers a UDP ASSOCIATE request with the relay's bound
// address and keeps the TCP connection open as the association's control
// channel (no TCP relay). OnClose terminates the association.
func (s *Server) setupUDPAssociate(c uio.Conn, st *connState) {
	// The request's DST.ADDR/DST.PORT is the client address the association
	// should expect datagrams from; an IP pins it exactly, 0.0.0.0 leaves only
	// the TCP-peer IP pin. The authenticated username rides to per-datagram Policy.
	var expect netip.AddrPort
	if ip, err := netip.ParseAddr(st.req.Host); err == nil {
		expect = netip.AddrPortFrom(ip, st.req.Port)
	}
	a, bnd, err := s.relay.associate(c, expect, st.username)
	if err != nil {
		s.dialFailures.Add(1)
		_, _ = c.Write(failureReply(RepGeneralFailure))
		_ = c.Close()
		return
	}
	st.udp = a
	var atyp byte = AtypIPv4
	if bnd.Addr().Is6() {
		atyp = AtypIPv6
	}
	_, _ = c.Write(appendReply(nil, RepSuccess, atyp, bnd.Addr().String(), bnd.Port()))
	s.connects.Add(1)
}

// repForDialError maps a dial failure to a SOCKS reply code.
func repForDialError(err error) byte {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return RepConnectionRefused
	case errors.Is(err, syscall.ENETUNREACH):
		return RepNetworkUnreachable
	case errors.Is(err, syscall.EHOSTUNREACH):
		return RepHostUnreachable
	case errors.Is(err, context.DeadlineExceeded):
		return RepTTLExpired
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return RepHostUnreachable
	}
	return RepGeneralFailure
}

// addrPortOf converts a net.Addr to a netip.AddrPort, tolerating a nil or
// non-IP address (the stdio backend) with a zero value.
func addrPortOf(addr net.Addr) netip.AddrPort {
	switch a := addr.(type) {
	case *net.TCPAddr:
		return a.AddrPort()
	case *net.UDPAddr:
		return a.AddrPort()
	}
	if addr != nil {
		if ap, err := netip.ParseAddrPort(addr.String()); err == nil {
			return ap
		}
	}
	return netip.AddrPort{}
}
