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
	"sync"
)

// Listener is an address an Events accepts on, added while it serves by
// Listen.
type Listener interface {
	// Addr is the address the listener is bound to; with a port of zero, the
	// port the system chose.
	Addr() net.Addr

	// Close stops accepting on the address and frees it. Stream connections
	// already accepted stay open. A UDP listener's peers share its socket, so
	// they close with it, with net.ErrClosed. Closing again does nothing.
	Close() error
}

// Listen adds addr, in Serve's syntax, to a serving Events and accepts on it
// at once, with the Handler, Observer and options of the rest. Every
// connection accepted there starts with userdata as its Userdata, the way
// Dial's userdata starts a dialed one. With ReusePort, the address gets the
// group of listeners Serve would give it. Before Serve has started, and once
// it is closing, Listen returns net.ErrClosed. Events.Close closes the
// listeners Listen added along with the others.
func (ev *Events) Listen(addr string, userdata any) (Listener, error) {
	if !ev.ready.Load() || ev.closing.Load() {
		return nil, net.ErrClosed
	}
	return ev.acceptor.listenGroup(addr, userdata)
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
