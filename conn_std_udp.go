//go:build windows || stdio

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

// Datagram connections on the portable backend: the shared UDP state, peer
// addressing and the child connection's reads.

package uio

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urpc/uio/internal/blockio"
)

func (fc *fdConn) moreInUDPBatch() bool { return false }

// fdConn uses one blocking reader and one blocking writer per stream. mux
// protects producer admission and counters; writeMu protects the detached
// batch while the writer performs blocking net.Conn writes; callbackMu keeps
// OnRead, Wake, and OnClose serialized for the connection.
// stdUDPState carries the datagram fields off the stream connections, which
// are the vast majority: sock is the shared packet socket of a server's peers
// or a connected client's socket, svr links a logical peer back to its
// server, and peers is the server's logical-peer map.
type stdUDPState struct {
	sock  *net.UDPConn
	svr   *fdConn
	peers map[netip.AddrPort]*fdConn

	// deadlines are a server child's own: the socket it shares with its
	// server and every other peer must not carry them. Allocated by the
	// first nonzero deadline; see applyChildDeadline.
	deadlines *childDeadlines

	// Datagrams are sent on whichever goroutine writes them, and the bytes
	// they report wait in outboundPending for whoever holds outboundClaim; see
	// reportOutbound.
	outboundPending atomic.Int64
	outboundClaim   atomic.Bool

	// A server's accepted close detaches the address table immediately so
	// DialPeer and WriteTo refuse it; the counted close task owns it until
	// physical release, even when the close queue has already stopped.
	closingPeers map[netip.AddrPort]*fdConn

	// controlling lists the goroutines inside a Control on a server's
	// socket, its peers' included. Closing the socket waits for their
	// descriptor references, so Listener.Close cannot close it on one of
	// them; see closeListenerSocket.
	controlMu   sync.Mutex
	controlling []int64
	// sealed refuses new Controls once a close found none running and went
	// on to close the socket: a Control that started after the check would
	// hold a descriptor reference that close waits for, and a Close inside
	// it would wait for that close in turn.
	sealed bool
}

// enterControl records the calling goroutine as inside a Control on the
// socket, until leaveControl; it refuses once a close sealed the socket.
func (udp *stdUDPState) enterControl() (int64, bool) {
	id := currentGoroutineID()
	udp.controlMu.Lock()
	defer udp.controlMu.Unlock()
	if udp.sealed {
		return 0, false
	}
	udp.controlling = append(udp.controlling, id)
	return id, true
}

func (udp *stdUDPState) leaveControl(id int64) {
	udp.controlMu.Lock()
	for i, held := range udp.controlling {
		if held == id {
			udp.controlling = append(udp.controlling[:i], udp.controlling[i+1:]...)
			break
		}
	}
	udp.controlMu.Unlock()
}

// controlsQuiet waits, bounded, for the Controls on the socket to end, and
// seals it against new ones as it finds none: the check and the seal are one
// step, so no Control slips in between it and the socket's close.
func (udp *stdUDPState) controlsQuiet() bool {
	deadline := time.Now().Add(socketCloseWait)
	for {
		udp.controlMu.Lock()
		held := len(udp.controlling)
		if held == 0 {
			udp.sealed = true
		}
		udp.controlMu.Unlock()
		if held == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// inControl reports whether the calling goroutine is inside a Control on the
// socket.
func (udp *stdUDPState) inControl() bool {
	id := currentGoroutineID()
	udp.controlMu.Lock()
	defer udp.controlMu.Unlock()
	for _, held := range udp.controlling {
		if held == id {
			return true
		}
	}
	return false
}

// peerKey is the canonical key of a UDP peer: an IPv4-mapped address
// collapses to its IPv4 form, so the dialing and receiving sides of one
// endpoint name it the same whatever form either was given.
func peerKey(addr netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
}

// dialPeer adds a child for addr to a UDP server; see Listener.DialPeer.
// The child is published under the server's mutex, and its OnOpen runs on a
// goroutine of its own serialized with the reader by the child's callback
// mutex; a datagram from the peer that arrives first runs it there instead.
func (fc *fdConn) dialPeer(addr netip.AddrPort, userdata any) (Conn, error) {
	if fc.entry != nil && !fc.entry.directConsumer {
		// A chain's peer connection is the stack's: the socket-level Conn
		// returned here would bypass every layer. Dial is the way in.
		return nil, ErrUnsupported
	}
	// The open joins the lifecycle before the peer is visible to any cleanup,
	// in one section with the shutdown publication: either the Events is
	// closing and none of this runs, or the open is counted and Serve's join
	// waits for it, so no callback slips past Serve's return.
	if err := fc.checkPeerAddr(addr); err != nil {
		return nil, err
	}
	events := fc.events()
	events.mux.Lock()
	if events.closing.Load() {
		events.mux.Unlock()
		return nil, net.ErrClosed
	}
	events.callbackWG.Add(1)
	events.mux.Unlock()

	fc.mux.Lock()
	if fc.udp == nil || fc.udp.peers == nil {
		fc.mux.Unlock()
		events.callbackWG.Done()
		return nil, net.ErrClosed
	}
	rAddr := peerKey(addr)
	if existing := fc.udp.peers[rAddr]; existing != nil && !existing.isClosing() {
		fc.mux.Unlock()
		events.callbackWG.Done()
		return existing, ErrPeerExists
	}
	// A closing peer's counted task owns its end even after replacement.
	child := &fdConn{}
	child.entry = fc.entry
	child.udp = &stdUDPState{sock: fc.udp.sock, svr: fc}
	child.addr = fc.addr
	child.setRemoteAddr(net.UDPAddrFromAddrPort(rAddr))
	child.loop = fc.loop
	child.SetUserdata(userdata)
	child.retainLifecycle()
	fc.udp.peers[rAddr] = child
	fc.mux.Unlock()
	go func() {
		blockio.EnterTurn()
		defer blockio.LeaveTurn()
		defer events.callbackWG.Done()
		child.openOnReader()
	}()
	return child, nil
}

// checkPeerAddr reports whether addr can be a peer of this UDP socket: the
// native backend's DialPeer refuses it in the address translation, and a
// child for an unusable address would come back to life only to fail every
// send.
func (fc *fdConn) checkPeerAddr(addr netip.AddrPort) error {
	ip := addr.Addr()
	if !ip.IsValid() {
		return fmt.Errorf("uio: dial peer %v: %w", addr, syscall.EINVAL)
	}
	local := fc.LocalAddr().(*net.UDPAddr)
	if local.IP.To4() != nil && !ip.Unmap().Is4() {
		return fmt.Errorf("uio: dial peer %v on udp4: %w", addr, syscall.EAFNOSUPPORT)
	}
	return nil
}

// writeToPeer sends one datagram to addr from a UDP server's socket; see
// Listener.WriteTo.
func (fc *fdConn) writeToPeer(b []byte, addr netip.AddrPort) (int, error) {
	fc.mux.Lock()
	peers := fc.udp.peers
	fc.mux.Unlock()
	if peers == nil {
		return 0, net.ErrClosed
	}
	return fc.udp.sock.WriteTo(b, net.UDPAddrFromAddrPort(addr))
}

// readUDPLoop owns a connected UDP socket. A datagram is borrowed for one
// callback and always discarded before the next blocking read.
func (fc *fdConn) readUDPLoop() {
	defer fc.events().callbackWG.Done()

	holder := fc.events().readPool.Get()
	defer fc.events().readPool.Put(holder)
	buffer := holder.bytes
	for {
		n, _, err := fc.udp.sock.ReadFrom(buffer)
		if nil != err {
			// close on error.
			fc.events().closeConn(fc, err)
			return
		}

		fc.callbackMu.Lock()
		if fc.isClosing() {
			// OnClose may already have run; see readLoop.
			fc.callbackMu.Unlock()
			return
		}
		fc.inboundTail = buffer[:n]

		// trigger inbound event.
		fc.events().onSocketBytesRead(fc, n)

		// fire data callback.
		started := fc.beginInboundCallback()
		if err = fc.events().onRead(fc); nil != err {
			fc.endInboundCallback(started)
			fc.callbackMu.Unlock()
			// close on error.
			fc.events().closeConn(fc, err)
			break
		}

		// drop unread udp packet.
		_, _ = fc.Discard(-1)
		fc.endInboundCallback(started)
		fc.callbackMu.Unlock()
	}
}

// listenUDP owns the shared packet socket and logical peer map. Each packet is
// borrowed for one callback and discarded before the receive buffer is reused.
func (fc *fdConn) listenUDP() error {
	blockio.EnterTurn()
	defer blockio.LeaveTurn()
	// UDP peer callbacks all run on this listener goroutine.
	defer fc.events().callbackWG.Done()

	holder := fc.events().readPool.Get()
	defer fc.events().readPool.Put(holder)
	buffer := holder.bytes

	for {
		n, addr, err := fc.udp.sock.ReadFrom(buffer)
		if nil != err {
			_ = fc.Abort(err)
			return err
		}

		// remote address, in the one form every side of a peer agrees on.
		rAddr := peerKey(remoteAddrFrom(addr))

		// udp server
		fc.mux.Lock()
		udpConn, ok := fc.udp.peers[rAddr]
		// A closed server leaves peers nil; a lookup on the nil map misses,
		// and inserting would panic. The datagram is dropped instead.
		if !ok && fc.udp.peers != nil {
			udpConn = &fdConn{}
			udpConn.entry = fc.entry
			udpConn.udp = &stdUDPState{sock: fc.udp.sock, svr: fc}
			udpConn.addr = fc.addr
			udpConn.setRemoteAddr(addr)
			udpConn.loop = fc.loop
			udpConn.SetUserdata(fc.Userdata()) // the listener's
			udpConn.retainLifecycle()

			fc.udp.peers[rAddr] = udpConn
		}
		fc.mux.Unlock()
		if udpConn == nil {
			continue
		}
		udpConn.callbackMu.Lock()
		// The open a new peer owes — or the one DialPeer still owes, when the
		// peer's datagram arrived before its opener — runs here, once.
		if !udpConn.opened {
			if !ok && testHookUDPChildCreated != nil {
				testHookUDPChildCreated(udpConn)
			}
			udpConn.ensureOpenLocked()
		}
		if udpConn.isClosing() {
			// The peer closed while this packet waited for its callback
			// mutex, and OnClose may already have run.
			udpConn.callbackMu.Unlock()
			continue
		}
		udpConn.inboundTail = buffer[:n]

		// trigger inbound event
		fc.events().onSocketBytesRead(udpConn, n)

		// fire udp on-data event.
		started := udpConn.beginInboundCallback()
		err = fc.events().onRead(udpConn)

		// drop unread udp packet.
		_, _ = udpConn.Discard(-1)
		udpConn.endInboundCallback(started)
		udpConn.callbackMu.Unlock()

		if nil != err {
			// close udp connection
			_ = udpConn.Abort(err)
		}
	}
}
