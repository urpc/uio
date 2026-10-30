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
	"context"
	"errors"
	"io"
	"net"
	"sync"
)

// defaultBridgeInbound bounds a leg's unconsumed bytes before the bridge
// pauses its reads.
const defaultBridgeInbound = 256 << 10

// bridgePeerMark tags the upstream leg the bridge dialed, so its OnOpen links
// it to its client instead of starting another dial.
type bridgePeerMark struct{ client Conn }

// bridgeLeg pairs the two connections of one relay. orphan marks a leg whose
// partner already closed: its inbound has nowhere to go and is discarded.
type bridgeLeg struct {
	peer   Conn
	orphan bool
}

// Bridge relays every connection of one Events to a peer it dials, both ways:
// bytes flow with end-to-end backpressure, a half-close is passed on, and an
// abort stays an abort.
//
// Every connection the bridge's Events accept is a client leg; the bridge
// dials its upstream and pairs them. A connection the application dials
// through the same Events is a client leg too — that is the outbound shape, a
// tunnel client dialing a relay and bridging the connection to a local
// service.
//
// Usage:
//
//	br := &uio.Bridge{Upstream: "127.0.0.1:9527"}
//	events := br.Events()
//	events.Serve(":9528")
//
// The bridge owns the Userdata of the upstream legs it dials; a client leg's
// user data is left alone. A Dial override may pick another address per
// client or add layers to the upstream (uio.WithLayers in its dial call).
type Bridge struct {
	// Upstream is the address every client leg is relayed to, dialed
	// through the bridge's own Events.
	Upstream string
	// Dial replaces the Upstream dial: it receives the client leg and a dial
	// function already bound to the bridge's Events (and to this client),
	// and returns the upstream leg. An error resets the client leg.
	Dial func(ctx context.Context, client Conn, dial DialFunc) (Conn, error)
	// OnPair is told each finished pairing.
	OnPair func(client, upstream Conn)
	// Observer, when set, hears the byte counts the bridge's legs report.
	Observer TrafficObserver
	// Hooks, when set, runs around the bridge's Events' Serve.
	Hooks ServeHooks
	// InboundLimit bounds the bytes a leg may leave unconsumed before the
	// bridge pauses that leg's reads (default 256 KiB). It also becomes the
	// Events' MaxInboundBuffered.
	InboundLimit int

	mu     sync.Mutex
	events *Events
	legs   map[Conn]*bridgeLeg
}

// DialFunc dials through the bridge's Events; the bridge adds the pairing
// mark to opts itself.
type DialFunc func(ctx context.Context, addr string, opts ...DialOption) (Conn, error)

// Pair links two connections the bridge already receives the callbacks of.
// A Dial override returning a connection it did not dial — a stream from a
// umux session configured with this bridge as its Handler, say — pairs it
// with Pair before returning, so a bridge can chain hops without a socket
// between them. Both connections must be handled by this bridge's Events or
// by a session whose Handler is this bridge.
func (b *Bridge) Pair(a, c Conn) {
	if a == nil || c == nil {
		return
	}
	if b.link(a, c) && b.OnPair != nil {
		b.OnPair(a, c)
	}
}

// errBridgePeerGone closes a leg whose partner was already closed when the
// bridge tried to pair them.
var errBridgePeerGone = errors.New("uio: paired connection already closed")

// link pairs two legs and then checks they are both still open. Registering
// first closes the race a check-then-register would leave open: a close that
// completes in that window finds no pair to release, and would otherwise leave
// the surviving leg waiting forever. On a dead leg, the registration is
// dropped and whichever leg is still live is reset.
func (b *Bridge) link(a, c Conn) bool {
	b.setPeer(a, c)
	aGone := a.State()&StateClosed != 0
	cGone := c.State()&StateClosed != 0
	if !aGone && !cGone {
		_ = a.Wake()
		_ = c.Wake()
		return true
	}
	b.unlink(a)
	if !aGone {
		b.abortLeg(a)
	}
	if !cGone {
		b.abortLeg(c)
	}
	return false
}

// unlink drops both entries of a pair without the orphan marking forget adds.
func (b *Bridge) unlink(c Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	leg := b.legs[c]
	delete(b.legs, c)
	if leg != nil && leg.peer != nil {
		delete(b.legs, leg.peer)
	}
}

// abortLeg resets a leg whose partner is gone.
func (b *Bridge) abortLeg(c Conn) {
	_ = c.SetLinger(0)
	_ = c.CloseWith(errBridgePeerGone)
}

// Events returns the Events the bridge needs: the bridge is its Handler,
// Observer and Hooks, half-open connections are on, and its inbound bound is
// InboundLimit. Configure Pollers, ReusePort and the rest on the returned
// value before Serve.
func (b *Bridge) Events() *Events {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.events != nil {
		return b.events
	}
	b.events = &Events{
		Handler:            b,
		Observer:           b,
		Hooks:              b,
		AllowHalfOpen:      true,
		MaxBufferSize:      64 << 10,
		MaxInboundBuffered: b.inboundLimit(),
	}
	return b.events
}

func (b *Bridge) inboundLimit() int {
	if b.InboundLimit > 0 {
		return b.InboundLimit
	}
	return defaultBridgeInbound
}

func (b *Bridge) peerOf(c Conn) Conn {
	b.mu.Lock()
	defer b.mu.Unlock()
	if leg := b.legs[c]; leg != nil {
		return leg.peer
	}
	return nil
}

// setPeer pairs two legs, updating both sides.
func (b *Bridge) setPeer(a, c Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.legs == nil {
		b.legs = make(map[Conn]*bridgeLeg)
	}
	la := b.legs[a]
	if la == nil {
		la = &bridgeLeg{}
		b.legs[a] = la
	}
	lc := b.legs[c]
	if lc == nil {
		lc = &bridgeLeg{}
		b.legs[c] = lc
	}
	la.peer = c
	lc.peer = a
}

// forget drops both entries of a leg's pair and reports the peer.
func (b *Bridge) forget(c Conn) Conn {
	b.mu.Lock()
	defer b.mu.Unlock()
	leg := b.legs[c]
	if leg == nil {
		return nil
	}
	delete(b.legs, c)
	peer := leg.peer
	if peer != nil {
		// The peer stays paired with nobody: it is orphaned, not dialing.
		b.legs[peer] = &bridgeLeg{orphan: true}
	}
	return peer
}

// orphaned reports a leg whose partner already closed.
func (b *Bridge) orphaned(c Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	leg := b.legs[c]
	return leg != nil && leg.orphan
}

// OnOpen dials the peer of a new client leg — or links the upstream leg the
// bridge dialed, recognized by its mark. It runs on the connection's turn, so
// a synchronous Dial delays that turn until the upstream is up.
func (b *Bridge) OnOpen(c Conn) {
	if mark, ok := c.Userdata().(*bridgePeerMark); ok && mark != nil {
		if b.link(mark.client, c) && b.OnPair != nil {
			b.OnPair(mark.client, c)
		}
		return
	}
	if peer := b.peerOf(c); peer != nil {
		// Paired before its OnOpen ran (a stream a Dial override paired, or
		// a Pair call): just let the held bytes flow.
		_ = c.Wake()
		_ = peer.Wake()
		return
	}
	b.mu.Lock()
	if b.legs == nil {
		b.legs = make(map[Conn]*bridgeLeg)
	}
	b.legs[c] = &bridgeLeg{}
	b.mu.Unlock()

	dial := func(ctx context.Context, addr string, opts ...DialOption) (Conn, error) {
		opts = append(opts, WithUserdata(&bridgePeerMark{client: c}))
		return b.Events().DialContext(ctx, addr, opts...)
	}
	var (
		up  Conn
		err error
	)
	switch {
	case b.Dial != nil:
		up, err = b.Dial(context.Background(), c, dial)
	case b.Upstream != "":
		up, err = dial(context.Background(), b.Upstream)
	default:
		err = errors.New("uio: bridge has no upstream")
	}
	if err != nil {
		b.forget(c)
		_ = c.SetLinger(0)
		_ = c.CloseWith(err)
		return
	}
	b.link(c, up)
}

// OnRead forwards a leg's inbound to its peer unless the peer's queue is
// backed up, and passes the end of the stream on.
func (b *Bridge) OnRead(c Conn) error {
	peer := b.peerOf(c)
	if peer == nil {
		if b.orphaned(c) {
			// The partner is gone: nothing can consume these bytes, and
			// holding them would keep the leg half-open forever.
			if _, err := c.Discard(-1); err != nil {
				return err
			}
			if c.State()&StatePeerEnded != 0 {
				// Graceful, not Close: queued bytes of this leg must still
				// reach the wire before it ends.
				_ = c.CloseGracefully(nil)
			}
		}
		return nil // the peer is not up yet: hold the bytes
	}
	// Leaving the bytes unconsumed is the backpressure: once enough pile up,
	// uio pauses this leg's reads and flow control slows the source.
	if peer.OutboundBuffered() < b.inboundLimit() {
		if _, err := c.WriteTo(peer); err != nil && !errors.Is(err, ErrOutboundOverflow) {
			if peerGone(peer, err) {
				// The pair is being torn down; its close path releases this leg.
				return nil
			}
			return err
		}
	}
	// The end of the stream is reported by the connection's state, not by a
	// Read probe: StatePeerEnded means the peer ended and everything it sent
	// was read.
	if c.State()&StatePeerEnded != 0 {
		if err := peer.CloseWrite(); err != nil && !peerGone(peer, err) {
			return err
		}
	}
	return nil
}

// OnTimer is a no-op: the bridge keeps no timers of its own.
func (b *Bridge) OnTimer(c Conn) error { return nil }

// OnClose passes an abort on as an abort; half-closes already traveled.
func (b *Bridge) OnClose(c Conn, err error) {
	peer := b.forget(c)
	if peer == nil {
		return
	}
	if err == nil || errors.Is(err, io.EOF) {
		// The leg ended cleanly: pass the end on. What still arrives for it is
		// discarded by the orphan path in OnRead, and the peer closes once it
		// ended too.
		_ = peer.CloseWrite()
		_ = peer.Wake()
		return
	}
	// CloseWith(err) aborts: a graceful Close would leave the peer's direction
	// open, and bytes still arriving for a client that is gone would never be
	// consumed.
	_ = peer.SetLinger(0)
	_ = peer.CloseWith(err)
}

// peerGone reports a peer the bridge can no longer deliver to: it closed or
// is closing. A forward that fails because of that says the pair is being torn
// down, not that the reading leg erred — failing the leg there would reset a
// half-close chain that is ending cleanly on both ends.
func peerGone(peer Conn, err error) bool {
	return errors.Is(err, net.ErrClosed) || peer.State()&(StateClosing|StateClosed) != 0
}

// OnQueueDrained hears a stream whose queued bytes a flush sent to the
// transport; a reader paused on that stream's queue can resume. The session
// reports it only to observers implementing this optional method.
func (b *Bridge) OnQueueDrained(c Conn) {
	if c.OutboundBuffered() < b.inboundLimit()/2 {
		if peer := b.peerOf(c); peer != nil {
			_ = peer.Wake()
		}
	}
}

// OnBytesRead forwards transport reads when an Observer is set.
func (b *Bridge) OnBytesRead(c Conn, n int) {
	if b.Observer != nil {
		b.Observer.OnBytesRead(c, n)
	}
}

// OnBytesWritten wakes a peer paused on this leg's queue, then reports.
func (b *Bridge) OnBytesWritten(c Conn, n int) {
	if c.OutboundBuffered() < b.inboundLimit()/2 {
		if peer := b.peerOf(c); peer != nil {
			_ = peer.Wake()
		}
	}
	if b.Observer != nil {
		b.Observer.OnBytesWritten(c, n)
	}
}

// OnStart records the Events when the application wired one by hand, then
// runs the configured hooks.
func (b *Bridge) OnStart(ev *Events) {
	b.mu.Lock()
	if b.events == nil {
		b.events = ev
	}
	hooks := b.Hooks
	b.mu.Unlock()
	if hooks != nil {
		hooks.OnStart(ev)
	}
}

// OnStop runs the configured hooks.
func (b *Bridge) OnStop(ev *Events) {
	if b.Hooks != nil {
		b.Hooks.OnStop(ev)
	}
}

var (
	_ Handler         = (*Bridge)(nil)
	_ TrafficObserver = (*Bridge)(nil)
	_ ServeHooks      = (*Bridge)(nil)
)
