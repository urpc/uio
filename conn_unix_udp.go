//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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

// Datagram connections on the native backend: the socket's UDP state, the
// read batch, and the per-peer child connections a server hands datagrams to.

package uio

import (
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/urpc/uio/internal/socket"
)

// unixUDPState contains fields that TCP connections never use. A UDP
// listener is a connection of its own, the server, and each peer it hears
// from is a child connection sharing its socket. The server's turns read
// every child's datagrams and run every child's callbacks, so peers belongs
// to them; a child closed or woken from elsewhere is queued for the server's
// next turn.
type unixUDPState struct {
	file   *os.File // owns a duplicated UDP listener fd when non-nil
	remote syscall.Sockaddr
	server *fdConn
	peers  map[socket.UDPAddress]*fdConn
	key    socket.UDPAddress

	family      uint8  // server only: the socket's address family, 4 or 6
	moreInBatch bool   // true while additional datagrams for this peer wait in the active recvmmsg batch
	groEnabled  bool   // true if UDP_GRO is enabled on this socket
	shardKey    uint32 // server only: 1-to-1 shard index in a ReusePort UDP group

	// peerMu guards a server's peers, which its turns read for every
	// datagram and DialPeer adds to from any goroutine, and its queues.
	peerMu  sync.Mutex
	closing []*fdConn // children whose close waits for the server's turn
	waking  []*fdConn // children whose Wake waits for the server's turn
	opening []*fdConn // children DialPeer made, whose OnOpen is owed

	// openOwed marks a child DialPeer made whose OnOpen has not run: the
	// server's turn, a datagram from the peer, or its close runs it first,
	// whichever claims it.
	openOwed atomic.Bool

	// batch stages the datagrams one read round takes from the socket; it
	// belongs to the reading turn. A child never reads.
	batch *udpBatch

	// Datagrams are sent on whichever goroutine writes them, and the bytes
	// they report wait in outboundPending for whoever holds outboundClaim; see
	// reportOutbound.
	outboundPending atomic.Int64
	outboundClaim   atomic.Bool

	// A server's socket closes on whichever goroutine gets there first —
	// Listener.Close's caller, so the address is free when it returns, or the
	// release in the server's turn — so every syscall on it, from any
	// goroutine, holds the gate; see releaseSocket.
	gate descriptorGate
	// closeMu and closed hold the socket close's single owner; see
	// releaseSocket.
	closeMu sync.Mutex
	closed  chan struct{}
}

// onRecvUDP drains the socket with bounded work per turn, and a read left
// owed when the budget runs out is redelivered to the next turn. A server's
// children share this socket and its peer map, so their callbacks run here.
func (conn *fdConn) onRecvUDP() error {
	slot := conn.events().readBufSize
	if conn.udp.groEnabled && slot < 64<<10 {
		slot = 64 << 10
	}
	if slot <= 0 {
		slot = 1
	}
	// The batch belongs to the reading socket and grows with the bursts it
	// sees: a quiet socket stages one datagram, a busy one a round's worth.
	if conn.udp.batch == nil {
		conn.udp.batch = newUDPBatch(slot, 1)
	}
	batch := conn.udp.batch

	var cachedAddr socket.UDPAddress
	var cachedConn *fdConn
	totalRead, packets := 0, 0
	for packets < 256 && totalRead < 1<<20 {
		want := min(len(batch.slots), 256-packets)
		gate, open := conn.enterDescriptor()
		if !open {
			return nil // The listener closed its socket; the release follows.
		}
		got, err := socket.RecvUDPBatch(int(conn.fd), batch.slots[:want], batch.recvs[:want])
		gate.leave()
		for i := 0; i < got; i++ {
			n := min(batch.recvs[i].Len, slot)
			totalRead += n
			addr := batch.recvs[i].Addr
			nextSamePeer := i+1 < got && (conn.udp.peers == nil || hasPeerInBatch(batch.recvs[i+1:got], addr))
			rawPkt := batch.slots[i][:n]
			gro := int(batch.recvs[i].GROSize)
			if gro > 0 && n > gro {
				for off := 0; off < n; off += gro {
					end := min(off+gro, n)
					packets++
					more := end < n || nextSamePeer
					cachedConn, cachedAddr = conn.handleUDPPacketCached(rawPkt[off:end], addr, more, cachedConn, cachedAddr)
					if conn.isClosing() {
						return nil
					}
				}
			} else {
				packets++
				cachedConn, cachedAddr = conn.handleUDPPacketCached(rawPkt, addr, nextSamePeer, cachedConn, cachedAddr)
				if conn.isClosing() {
					return nil
				}
			}
		}
		if err != nil {
			if isWouldBlock(err) {
				return nil
			}
			return err
		}
		if got < want {
			return nil // drained
		}
		if got == len(batch.slots) {
			batch.grow(slot)
		}
	}
	if !conn.isClosing() {
		conn.taskState.Or(ioEventRead)
	}
	return nil
}

// udpBatch stages one UDP read round: a slot of readBufferSize per datagram,
// the vectors pointing into them, and their sources. It starts at one slot
// and doubles after full rounds, so a quiet socket holds a read's worth and a
// busy one a round's.
type udpBatch struct {
	data  []byte
	slots [][]byte
	recvs []socket.UDPReceive
}

func newUDPBatch(slotSize, count int) *udpBatch {
	batch := &udpBatch{
		data:  make([]byte, slotSize*count),
		slots: make([][]byte, count),
		recvs: make([]socket.UDPReceive, count),
	}
	for i := range batch.slots {
		batch.slots[i] = batch.data[i*slotSize : (i+1)*slotSize]
	}
	return batch
}

func hasPeerInBatch(recvs []socket.UDPReceive, addr socket.UDPAddress) bool {
	for i := range recvs {
		if recvs[i].Addr.Port == addr.Port && recvs[i].Addr == addr {
			return true
		}
	}
	return false
}

func (conn *fdConn) moreInUDPBatch() bool {
	return conn.udp != nil && conn.udp.moreInBatch
}

// handleUDPPacket resolves or creates the logical peer connection and lends
// the packet buffer only for the duration of its synchronous callbacks.
func (conn *fdConn) handleUDPPacket(packet []byte, source socket.UDPAddress) {
	_, _ = conn.handleUDPPacketCached(packet, source, false, nil, socket.UDPAddress{})
}

func (conn *fdConn) handleUDPPacketCached(packet []byte, source socket.UDPAddress, more bool, prev *fdConn, prevAddr socket.UDPAddress) (*fdConn, socket.UDPAddress) {
	packetConn := conn
	if conn.udp.peers != nil {
		if prev != nil && source == prevAddr && !prev.isClosing() {
			packetConn = prev
			if packetConn.udp.openOwed.CompareAndSwap(true, false) {
				packetConn.fireOnOpen()
			}
		} else {
			// UDP children are logical connections that share the server fd.
			udp := conn.udp
			udp.peerMu.Lock()
			packetConn = udp.peers[source]
			created := false
			if packetConn == nil && udp.peers != nil {
				sockaddr := source.Sockaddr()
				if sockaddr == nil {
					udp.peerMu.Unlock()
					return nil, socket.UDPAddress{}
				}
				packetConn = &fdConn{fd: conn.fd, udp: &unixUDPState{remote: sockaddr, server: conn, key: source}}
				packetConn.entry = conn.entry
				packetConn.loop = conn.loop
				packetConn.SetUserdata(conn.Userdata()) // the listener's
				packetConn.addr, packetConn.remoteAddr = conn.addr, source.NetAddr().AddrPort()
				udp.peers[source] = packetConn
				created = true
			}
			udp.peerMu.Unlock()
			if packetConn == nil {
				return nil, socket.UDPAddress{}
			}
			if created || packetConn.udp.openOwed.CompareAndSwap(true, false) {
				packetConn.fireOnOpen()
			}
		}
	}
	if packetConn.isClosing() {
		return nil, socket.UDPAddress{}
	}
	packetConn.udp.moreInBatch = more
	packetConn.inboundTail = packet
	packetConn.events().onSocketBytesRead(packetConn, len(packet))
	var err error
	func() {
		defer func() {
			started := packetConn.beginInboundCallback()
			_, _ = packetConn.Discard(-1)
			packetConn.endInboundCallback(started)
			packetConn.inboundTail = nil
			packetConn.udp.moreInBatch = false
		}()
		err = packetConn.fireOnRead()
	}()
	if err != nil {
		packetConn.requestClose(err)
		return nil, socket.UDPAddress{}
	}
	return packetConn, source
}

// dialPeer adds a child for addr to a UDP server; see Listener.DialPeer.
func (server *fdConn) dialPeer(addr netip.AddrPort, userdata any) (Conn, error) {
	if server.entry != nil && !server.entry.directConsumer {
		return nil, ErrUnsupported
	}
	udp := server.udp
	key, err := socket.UDPAddressFor(udp.family, addr)
	if err != nil {
		return nil, err
	}
	sockaddr := key.Sockaddr()
	udp.peerMu.Lock()
	if udp.peers == nil || server.isClosing() || server.events().closing.Load() {
		udp.peerMu.Unlock()
		return nil, net.ErrClosed
	}
	if existing := udp.peers[key]; existing != nil {
		if !existing.isClosing() {
			udp.peerMu.Unlock()
			return existing, ErrPeerExists
		}
		// Transfer ownership before replacing the address entry. The old
		// closer may still be between marking closing and queuePeer, so the
		// server must retain it even if shutdown wins before that hand-off.
		udp.closing = append(udp.closing, existing)
	}
	child := &fdConn{fd: server.fd, udp: &unixUDPState{remote: sockaddr, server: server, key: key}}
	child.entry = server.entry
	child.loop = server.loop
	child.addr, child.remoteAddr = server.addr, key.NetAddr().AddrPort()
	child.SetUserdata(userdata)
	child.udp.openOwed.Store(true)
	// A closing child still in the map is replaced: its release leaves the
	// new one there.
	udp.peers[key] = child
	udp.opening = append(udp.opening, child)
	udp.peerMu.Unlock()
	server.scheduleIO(ioEventPeers)
	return child, nil
}

// writeToPeer sends one datagram to addr from a UDP server's socket; see
// Listener.WriteTo.
func (server *fdConn) writeToPeer(b []byte, addr netip.AddrPort) (int, error) {
	key, err := socket.UDPAddressFor(server.udp.family, addr)
	if err != nil {
		return 0, err
	}
	sockaddr := key.Sockaddr()
	server.submitMu.Lock()
	defer server.submitMu.Unlock()
	if server.isClosing() || server.close.isReleased() || server.events().closing.Load() {
		return 0, net.ErrClosed
	}
	if !server.udp.gate.enter() {
		return 0, net.ErrClosed
	}
	err = syscall.Sendto(int(server.fd), b, 0, sockaddr)
	server.udp.gate.leave()
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// queuePeer hands a child's close or Wake to the server's turn, which owns
// the children: wake selects which.
func (server *fdConn) queuePeer(child *fdConn, wake bool) {
	udp := server.udp
	udp.peerMu.Lock()
	if udp.peers == nil {
		udp.peerMu.Unlock()
		return // The release pass already owns every child and queued request.
	}
	if wake {
		udp.waking = append(udp.waking, child)
	} else {
		udp.closing = append(udp.closing, child)
	}
	udp.peerMu.Unlock()
	server.scheduleIO(ioEventPeers)
}

// settlePeers runs in a UDP server's turn and handles the children queued for
// it: a closing child is released and gets OnClose, and a woken one OnRead.
// Children queued meanwhile set ioEventPeers again for the next turn.
//
// Children are taken one at a time, and ioEventPeers stays set while more are
// queued: a callback panic that an Executor recovers ends this turn, and the
// next one takes the rest.
func (server *fdConn) settlePeers() {
	udp := server.udp
	// Process only the requests that were queued when this turn reached the
	// peer hand-off. A callback may call Wake, which queues another request for
	// the next turn; draining that request here would let a self-waking UDP
	// callback keep this turn alive forever and prevent shutdown from joining
	// it.
	udp.peerMu.Lock()
	remaining := len(udp.closing) + len(udp.waking)
	opening := udp.opening
	udp.opening = nil
	udp.peerMu.Unlock()
	for _, child := range opening {
		if child.udp.openOwed.CompareAndSwap(true, false) {
			child.fireOnOpen()
		}
	}
	for processed := 0; processed < remaining; processed++ {
		udp.peerMu.Lock()
		var child *fdConn
		wake := false
		if len(udp.closing) != 0 {
			child = udp.closing[0]
			udp.closing[0] = nil
			udp.closing = udp.closing[1:]
		} else if len(udp.waking) != 0 {
			child, wake = udp.waking[0], true
			udp.waking[0] = nil
			udp.waking = udp.waking[1:]
		}
		if child == nil {
			udp.peerMu.Unlock()
			break
		}
		udp.peerMu.Unlock()
		if !wake {
			if finalErr, ok := child.teardown(nil); ok {
				child.deliverClose(finalErr)
			}
		} else if !child.isClosing() {
			if err := child.fireOnRead(); err != nil {
				child.requestClose(err)
			}
		}
	}
	// Keep the synthetic event set when a callback queued more work while this
	// turn was running. finishIOTask will hand it to a fresh turn after the
	// current one returns.
	udp.peerMu.Lock()
	if len(udp.closing)+len(udp.waking)+len(udp.opening) == 0 {
		server.taskState.And(^ioEventPeers)
	} else {
		server.taskState.Or(ioEventPeers)
	}
	udp.peerMu.Unlock()
}
