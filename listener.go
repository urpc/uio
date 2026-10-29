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

package uio

import (
	"net"
	"net/netip"
	"strings"
	"sync"
)

// Listener is an address an Events accepts on, added while it serves by
// Listen.
type Listener interface {
	// Addr is the address the listener is bound to; with a port of zero, the
	// port the system chose.
	Addr() net.Addr

	// Close stops accepting on the address and frees it: once Close returns,
	// the address can be bound again. Stream connections already accepted
	// stay open. A UDP listener's peers share its socket, so they close with
	// it, with net.ErrClosed. Two cases free a UDP address only after Close
	// returns: a Close from a callback of one of the listener's own peers or
	// from inside a Control on its socket frees it once that returns, and a
	// Control still running elsewhere holds it until it returns. Closing again
	// does nothing.
	Close() error
}

// PacketListener is the Listener of a packet address. Assert it off the
// Listener Listen returned: only a packet listener's carries it.
type PacketListener interface {
	Listener

	// DialPeer adds a peer connection addressed at raddr, on the listener's
	// own socket: what it sends leaves from the listener's address, and the
	// datagrams raddr answers with are delivered to it — as they would be
	// had raddr spoken first. It is the entry point for hole punching. The
	// returned Conn is writable at once, before OnOpen, which runs once in a
	// turn of its own and is always followed by OnClose. A peer that spoke
	// first already has its connection: it is returned with ErrPeerExists,
	// keeping the userdata it opened with. userdata is the new connection's
	// initial Userdata. Before any datagram from raddr arrives, a closed
	// child is replaced: a peer whose old connection is closing can be dialed
	// again.
	//
	// A listener that carries a layer chain refuses DialPeer with
	// ErrUnsupported: the peer's connection is the stack's, which only the
	// stack opens, and the Conn returned here would bypass every layer.
	DialPeer(raddr netip.AddrPort, userdata any) (Conn, error)

	// WriteTo sends one datagram to raddr from the listener's socket, from
	// the listener's own address, without a connection. It reports the bytes
	// sent, not their arrival.
	WriteTo(b []byte, raddr netip.AddrPort) (int, error)
}

// Listen adds addr, in Serve's syntax, to a serving Events and accepts on it
// at once, with the Handler, Observer and options of the rest. Every
// connection accepted there starts with the Userdata a WithUserdata option
// names, the way a dial's names its connection's. opts configure this
// listener: the accepted connections' Userdata (WithUserdata), its chain
// (WithLayers) and its sockets (WithListenControl). With ReusePort, the
// address gets the group of listeners Serve would give it. A unix path is
// unlinked and bound afresh, taking the socket file over from whoever held
// it. Listen binds even from ServeHooks.OnStart, and accepting on such a
// listener starts once OnStart returns, so a server binds its ports and
// their chains before any callback runs. Before Serve has started, and once
// it is closing, Listen returns net.ErrClosed. Events.Close closes the
// listeners Listen added along with the others.
func (ev *Events) Listen(addr string, opts ...ListenOption) (Listener, error) {
	if !ev.ready.Load() || ev.closing.Load() {
		return nil, net.ErrClosed
	}
	network := "tcp"
	if scheme, _, ok := strings.Cut(addr, "://"); ok {
		network = scheme
	}
	cfg := listenConfigFor(opts)
	if err := refuseLayerPacket(network, cfg.layers, cfg.hasLayers); err != nil {
		return nil, err
	}
	// Socket creation can call application controls. The acceptor admits the
	// resulting sockets under its own shutdown lock, after those calls return.
	return ev.acceptor.listenGroup(addr, cfg)
}

// listenerGroup is the Listener of one Listen: the listeners it created for
// its address, one or a ReusePort group.
type listenerGroup struct {
	ld      *acceptor
	members []*listener
	addr    net.Addr
	once    sync.Once
}

func (g *listenerGroup) Addr() net.Addr { return g.addr }

func (g *listenerGroup) Close() error {
	g.once.Do(func() { g.ld.closeGroup(g.members) })
	return nil
}

// dialPeerAt and writeToPeerAt reach the packet methods without giving
// *listenerGroup the PacketListener method set: only packetListener has it,
// so a stream listener's Listener asserts to no more than Listener.
func (g *listenerGroup) dialPeerAt(raddr netip.AddrPort, userdata any) (Conn, error) {
	return g.members[0].udpSvr.dialPeer(raddr, userdata)
}

func (g *listenerGroup) writeToPeerAt(b []byte, raddr netip.AddrPort) (int, error) {
	return g.members[0].udpSvr.writeToPeer(b, raddr)
}

// packetListener is the PacketListener of a packet listener group; a stream
// listener's Listener is the bare group, which asserts to no more.
type packetListener struct{ group *listenerGroup }

func (p packetListener) Addr() net.Addr { return p.group.Addr() }
func (p packetListener) Close() error   { return p.group.Close() }
func (p packetListener) DialPeer(raddr netip.AddrPort, userdata any) (Conn, error) {
	return p.group.dialPeerAt(raddr, userdata)
}

func (p packetListener) WriteTo(b []byte, raddr netip.AddrPort) (int, error) {
	return p.group.writeToPeerAt(b, raddr)
}

// asListener picks a group's Listener face: its PacketListener face for a
// packet address, the bare Listener of the group for a stream one.
func (g *listenerGroup) asListener() Listener {
	if g.members[0].udpSvr != nil {
		return packetListener{group: g}
	}
	return g
}
