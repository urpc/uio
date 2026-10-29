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

package ukcp

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/urpc/uio"
)

// DefaultDialer is the Dialer the package-level Dial uses. It runs with the
// default options, Handler among them, and lives until the process ends, like
// http.DefaultClient: set its Handler before dialing through Dial, and close
// it — which closes the connections it made — before the process goes.
var DefaultDialer = &Dialer{}

// Dial connects through DefaultDialer with no deadline; see Dialer.Dial.
func Dial(addr string) (uio.Conn, error) {
	return DefaultDialer.Dial(addr)
}

// Dialer dials KCP sessions through one private UIO transport, which the
// first dial starts and Close stops: sessions share the Events — and with it
// the Handler their callbacks reach — and close with it. Dial bounds the dial
// by nothing, DialContext by its context. The dialed connection is one
// reliable stream, read through the Handler as any uio connection is; the
// connection returned is for writing, closing and addressing. For TLS over
// KCP, use the layer form — ClientLayer above utls.Client in a WithLayers
// chain — as with any other protocol.
type Dialer struct {
	Options

	mu     sync.Mutex
	events *uio.Events
	closed bool
}

// NewDialer returns a Dialer using opts, http.Server-style: the exported
// fields are the configuration, this constructor only fills them in.
func NewDialer(opts Options) *Dialer { return &Dialer{Options: opts} }

// Dial is DialContext with no deadline, as a server's Serve takes none.
func (d *Dialer) Dial(addr string) (uio.Conn, error) {
	return d.DialContext(context.Background(), addr)
}

// DialContext dials addr, in Serve's syntax — no scheme means udp — through
// the KCP layer, returning once the connection's stack opened; a client needs
// no handshake of its own, so this does not wait for the peer to answer, and
// the first segments retransmit until it does. ctx bounds the dial, not the
// connection after it. A dial after Close reports net.ErrClosed.
func (d *Dialer) DialContext(ctx context.Context, addr string) (uio.Conn, error) {
	resolved, err := d.Options.resolve()
	if err != nil {
		return nil, err
	}
	if resolved.Handler == nil {
		return nil, fmt.Errorf("%w: a Dialer needs a Handler: reads arrive through it", ErrInvalidOptions)
	}
	if err := resolved.checkTransport(); err != nil {
		return nil, err
	}
	events, err := d.transport(resolved)
	if err != nil {
		return nil, err
	}
	chain := []uio.Layer{newLayer(resolved, true)}
	dialOpts := []uio.DialOption{uio.WithLayers(chain...)}
	if control := resolved.socketControl(); control != nil {
		dialOpts = append(dialOpts, uio.WithDialControl(control))
	}
	return events.DialContext(ctx, withKCPScheme(addr), dialOpts...)
}

// Close stops the dialer's transport, and the sessions it made with it;
// dialing again reports net.ErrClosed. Closing again does nothing.
func (d *Dialer) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	events := d.events
	d.events = nil
	d.mu.Unlock()
	if events == nil {
		return nil
	}
	return events.Close(nil)
}

// transport is the dialer's Events, started on first use.
func (d *Dialer) transport(opts Options) (*uio.Events, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, net.ErrClosed
	}
	if d.events != nil {
		return d.events, nil
	}
	started := make(chan struct{})
	events := &uio.Events{
		Pollers:                opts.Pollers,
		Executor:               opts.Executor,
		MaxBufferSize:          opts.MaxBufferSize,
		WriteBufferedThreshold: opts.WriteBufferedThreshold,
		MaxOutboundBuffered:    opts.MaxOutboundBuffered,
		AllowHalfOpen:          opts.AllowHalfOpen,
		Handler:                opts.Handler,
		Hooks:                  uio.FuncHandler{Start: func(*uio.Events) { close(started) }},
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	select {
	case <-started:
	case err := <-done:
		return nil, err
	}
	d.events = events
	return events, nil
}
