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

// Package ubridge relays bidirectional stream connections with end-to-end
// backpressure, half-close propagation, and abort forwarding.
package ubridge

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/urpc/uio"
	"github.com/urpc/uio/internal/readcontrol"
	"github.com/urpc/uio/internal/writecontrol"
)

// defaultBridgeInbound is a leg's default receive bound and its destination
// queue's high watermark.
const defaultBridgeInbound = 256 << 10

// bridgePeerMark tags an upstream leg the bridge's dial helper made: until
// the client's OnOpen pairs it, its bytes wait for the pair.
type bridgePeerMark struct{}

// bridgeLeg is one connection of a relay, as a source: what it waits for
// when its destination is behind.
type bridgeLeg struct {
	conn uio.Conn
	// pause is the bridge's own pause of this leg, nil for a connection that
	// has none (a custom Conn); see Bridge.hold.
	pause *readcontrol.Pause
	// waiting marks input this leg left unforwarded because its destination
	// was behind; the destination's drain claims it and lets the leg go on.
	waiting atomic.Bool
	// drains counts the destination's drains to its low watermark, so a leg
	// that starts waiting can tell whether one came after it last forwarded.
	drains atomic.Uint64
}

// bridgePair is one relay: two legs, each the other's destination.
type bridgePair struct {
	a, b   bridgeLeg
	closed atomic.Bool
}

// legs returns c's leg and the other one.
func (p *bridgePair) legs(c uio.Conn) (own, other *bridgeLeg) {
	if c == p.a.conn {
		return &p.a, &p.b
	}
	return &p.b, &p.a
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
//	br := &ubridge.Bridge{Upstream: "127.0.0.1:9527"}
//	events := br.Events()
//	events.Serve(":9528")
//
// The bridge owns the Userdata of the upstream legs its dial helper makes; a
// client leg's user data is left alone. A Dial override may pick another
// address per client, add layers to the upstream (uio.WithLayers in its dial
// call), or return a connection it did not dial — a umux stream, say.
//
// Backpressure: a leg whose destination queue reaches InboundLimit leaves its
// input unconsumed, and its receive bound stops its socket reads; the
// destination draining to half the limit lets it forward again. A leg that
// fills its own bound while it waits is also paused, with a pause of the
// bridge's own, so even an Events without MaxInboundBuffered stays bounded.
// The application's PauseRead switch is independent of it.
type Bridge struct {
	// Upstream is the address every client leg is relayed to, dialed
	// through the bridge's own Events.
	Upstream string
	// Dial replaces the Upstream dial: it receives the client leg and a dial
	// function already bound to the bridge's Events, and returns the upstream
	// leg, which the bridge pairs with the client once Dial returns. It may
	// use the dial function once; a connection it dialed but did not return
	// is closed. An error resets the client leg.
	Dial func(ctx context.Context, client uio.Conn, dial DialFunc) (uio.Conn, error)
	// OnPair is told each finished pairing.
	OnPair func(client, upstream uio.Conn)
	// Observer, when set, hears the byte counts the bridge's legs report.
	Observer uio.TrafficObserver
	// Hooks, when set, runs around the bridge's Events' Serve.
	Hooks uio.ServeHooks
	// InboundLimit bounds the bytes a leg leaves unconsumed and is the high
	// watermark of its destination's queue (default 256 KiB). It also
	// becomes the Events' MaxInboundBuffered.
	InboundLimit int

	mu     sync.Mutex
	events *uio.Events
	pairs  map[uio.Conn]*bridgePair
}

// DialFunc dials through the bridge's Events; the bridge adds its own mark
// to opts.
type DialFunc func(ctx context.Context, addr string, opts ...uio.DialOption) (uio.Conn, error)

var (
	// ErrPeerGone closes a leg whose partner was already closed when the
	// bridge tried to pair them.
	ErrPeerGone = errors.New("ubridge: paired connection already closed")
	// ErrAlreadyPaired refuses a pairing with a connection already relayed.
	ErrAlreadyPaired = errors.New("ubridge: connection already belongs to another bridge pair")
)

func resolveControlBase(c uio.Lower) uio.Lower {
	for c != nil {
		if readcontrol.ControlOf(c) != nil || writecontrol.ControlOf(c) != nil {
			return c
		}
		next := uio.Unwrap(c)
		if next == nil {
			return c
		}
		c = next
	}
	return nil
}

// Pair links two connections the bridge already receives the callbacks of:
// both must be handled by this bridge's Events or by a umux session whose
// Handler is this bridge. A Dial override's returned connection is paired
// this way by the bridge itself. Repeating a pairing is harmless; a
// connection already paired with another is refused with ErrAlreadyPaired.
// On failure the caller remains responsible for closing its connections.
func (b *Bridge) Pair(a, c uio.Conn) error {
	if a == nil || c == nil || a == c {
		return uio.ErrUnsupported
	}
	baseA, baseC := resolveControlBase(a), resolveControlBase(c)
	p := &bridgePair{
		a: bridgeLeg{conn: a, pause: readcontrol.NewPause(baseA)},
		b: bridgeLeg{conn: c, pause: readcontrol.NewPause(baseC)},
	}
	b.mu.Lock()
	if b.pairs == nil {
		b.pairs = make(map[uio.Conn]*bridgePair)
	}
	if old := b.pairs[a]; old != nil || b.pairs[c] != nil {
		same := old != nil && old == b.pairs[c] && !old.closed.Load()
		b.mu.Unlock()
		if same {
			return nil
		}
		return ErrAlreadyPaired
	}
	if bridgeConnGone(a) || bridgeConnGone(c) {
		b.mu.Unlock()
		return ErrPeerGone
	}
	b.pairs[a], b.pairs[c] = p, p
	b.mu.Unlock()
	// A close that completed before the pair was published could not find
	// it, and its OnClose does not repeat: look again now that it is. If
	// OnClose already claimed p.closed after publication, let OnClose finish.
	if (bridgeConnGone(a) || bridgeConnGone(c)) && p.closed.CompareAndSwap(false, true) {
		b.mu.Lock()
		delete(b.pairs, a)
		delete(b.pairs, c)
		b.mu.Unlock()
		return ErrPeerGone
	}
	if b.OnPair != nil {
		b.OnPair(a, c)
	}
	writecontrol.Watch(baseA, func(int) { b.resumeSource(a) })
	writecontrol.Watch(baseC, func(int) { b.resumeSource(c) })
	// Bytes either leg received before the pairing wait for a turn.
	_ = a.Wake()
	_ = c.Wake()
	return nil
}

func (b *Bridge) pairOf(c uio.Conn) *bridgePair {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pairs[c]
}

func (b *Bridge) inboundLimit() int {
	if b.InboundLimit > 0 {
		return b.InboundLimit
	}
	return defaultBridgeInbound
}

// Events returns the Events the bridge needs: the bridge is its Handler,
// Observer and Hooks, half-open connections are on, and its inbound bound is
// InboundLimit. Configure Pollers, ReusePort and the rest on the returned
// value before Serve.
func (b *Bridge) Events() *uio.Events {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.events == nil {
		b.events = &uio.Events{
			Handler:            b,
			Observer:           b,
			Hooks:              b,
			AllowHalfOpen:      true,
			ReadBufferSize:     64 << 10,
			MaxInboundBuffered: b.inboundLimit(),
		}
	}
	return b.events
}

// bridgeAbort resets a leg the relay cannot carry on.
func bridgeAbort(c uio.Conn, err error) {
	if c != nil {
		if sc, ok := uio.SocketOf(c); ok {
			_ = sc.SetLinger(0)
		}
		_ = c.Abort(err)
	}
}

// OnOpen dials the upstream of a new client leg and pairs the two once the
// dial returned. An upstream the dial helper made waits for that pairing.
// It runs on the connection's turn, so a synchronous Dial delays that turn
// until the upstream is up.
func (b *Bridge) OnOpen(c uio.Conn) {
	if _, upstream := c.Userdata().(bridgePeerMark); upstream {
		return
	}
	if b.pairOf(c) != nil {
		// Paired before its OnOpen ran — a stream a Dial override returned,
		// or a Pair call: bytes it held flow now.
		_ = c.Wake()
		return
	}
	var helper uio.Conn
	dial := func(ctx context.Context, addr string, opts ...uio.DialOption) (uio.Conn, error) {
		opts = append(opts[:len(opts):len(opts)], uio.WithUserdata(bridgePeerMark{}))
		up, err := b.Events().DialContext(ctx, addr, opts...)
		helper = up
		return up, err
	}
	var up uio.Conn
	var err error
	switch {
	case b.Dial != nil:
		up, err = b.Dial(context.Background(), c, dial)
	case b.Upstream != "":
		up, err = dial(context.Background(), b.Upstream)
	default:
		err = errors.New("ubridge: bridge has no upstream")
	}
	if err == nil {
		err = b.Pair(c, up)
	}
	if err != nil {
		if b.pairOf(c) == nil {
			bridgeAbort(c, err)
		} // else the override paired it elsewhere: that pair owns it now.
		if up != c && (!errors.Is(err, ErrAlreadyPaired) || b.pairOf(up) == nil) {
			bridgeAbort(up, err)
		}
		if helper != c && helper != up && b.pairOf(helper) == nil {
			bridgeAbort(helper, err)
		}
		return
	}
	if helper != nil && helper != up {
		bridgeAbort(helper, ErrPeerGone) // dialed, then not returned
	}
}

// OnRead forwards a leg's input to its destination while that destination's
// queue is below InboundLimit, and passes the end of the stream on.
// Otherwise the input stays unconsumed until the destination drains to half
// the limit; see resumeSource.
func (b *Bridge) OnRead(c uio.Conn) error {
	p := b.pairOf(c)
	if p == nil {
		return nil // not paired yet: the pairing wakes it
	}
	if p.closed.Load() {
		// The partner is gone: nothing can consume these bytes, and holding
		// them would keep the leg half-open forever.
		if _, err := c.Discard(-1); err != nil {
			return err
		}
		if c.State()&uio.StateReadEnded != 0 {
			_ = c.Close()
		}
		return nil
	}
	own, other := p.legs(c)
	peer := other.conn
	high := b.inboundLimit()
	drains := own.drains.Load()
	if peer.OutboundBuffered() < high {
		n, err := c.WriteTo(peer)
		if n == 0 && errors.Is(err, uio.ErrOutboundOverflow) && peer.OutboundBuffered() == 0 {
			// A chunk larger than the empty destination's whole budget:
			// forward a prefix it takes, the rest on its drains.
			_, err = c.WriteTo(bridgePrefixWriter{peer})
		}
		if err != nil && !errors.Is(err, uio.ErrOutboundOverflow) {
			if peerGone(peer, err) {
				return nil // the pair is being torn down; its close releases this leg
			}
			return err
		}
	}
	if c.InboundBuffered() == 0 {
		// StateReadEnded means the peer ended and everything it sent was
		// read: the end follows the bytes.
		if c.State()&uio.StateReadEnded != 0 {
			if err := peer.CloseWrite(); err != nil && !errors.Is(err, uio.ErrUnsupported) && !peerGone(peer, err) {
				return err
			}
		}
		return nil
	}
	// The destination is behind. A drain claims the waiting mark and runs
	// this callback again; one that came before the mark was set did not see
	// it, so this leg goes on by itself.
	own.waiting.Store(true)
	if own.drains.Load() != drains {
		if own.waiting.CompareAndSwap(true, false) {
			_ = c.Wake()
		}
		return nil
	}
	if c.InboundBuffered() >= high {
		b.hold(own)
	}
	return nil
}

// hold pauses a waiting leg whose unforwarded input reached the limit, so an
// Events without a receive bound stops reading it too. A drain that claimed
// the waiting mark before the pause was published could not release it, so
// the pause looks once more.
func (b *Bridge) hold(leg *bridgeLeg) {
	if leg.pause.Pause() != nil {
		return
	}
	if !leg.waiting.Load() {
		_ = leg.pause.Resume()
	}
}

// resumeSource hears a destination's queue drain to half the limit or below
// and lets the leg that waited on it go on: a paused one through the
// pause's release, which runs the callback with what it retained.
func (b *Bridge) resumeSource(destination uio.Conn) {
	if destination.OutboundBuffered() > b.inboundLimit()/2 {
		return
	}
	p := b.pairOf(destination)
	if p == nil {
		return
	}
	_, source := p.legs(destination)
	source.drains.Add(1)
	if !source.waiting.CompareAndSwap(true, false) {
		return
	}
	if source.pause.Paused() {
		_ = source.pause.Resume()
	} else {
		_ = source.conn.Wake()
	}
}

// bridgePrefixWriter halves a write an empty destination refuses whole until
// a prefix fits. Keeping the overflow error makes WriteTo retain exactly the
// unsent suffix, which the destination's drains forward.
type bridgePrefixWriter struct{ destination uio.Conn }

func (w bridgePrefixWriter) Write(p []byte) (int, error) {
	want := len(p)
	for {
		n, err := w.destination.Write(p)
		if n != 0 || !errors.Is(err, uio.ErrOutboundOverflow) || len(p) <= 1 || w.destination.OutboundBuffered() != 0 {
			if err == nil && len(p) < want {
				err = uio.ErrOutboundOverflow
			}
			return n, err
		}
		p = p[:len(p)/2]
	}
}

// OnTimer is a no-op: the bridge keeps no timers of its own.
func (b *Bridge) OnTimer(uio.Conn) error { return nil }

// OnClose passes an abort on as an abort and a clean end as the end of the
// partner's writing half; the partner then discards what still arrives for
// the gone leg and closes once it ended too.
func (b *Bridge) OnClose(c uio.Conn, err error) {
	b.mu.Lock()
	p := b.pairs[c]
	delete(b.pairs, c)
	b.mu.Unlock()
	if p == nil || p.closed.Swap(true) {
		return
	}
	_, other := p.legs(c)
	peer := other.conn
	if err == nil || errors.Is(err, io.EOF) {
		if cwErr := peer.CloseWrite(); errors.Is(cwErr, uio.ErrUnsupported) {
			_ = peer.Close()
		}
		// Its next callback takes the orphan path; a pause of the bridge's
		// own would keep that from running.
		other.waiting.Store(false)
		if other.pause.Paused() {
			_ = other.pause.Resume()
		} else {
			_ = peer.Wake()
		}
		return
	}
	// Abort(err) resets: a graceful Close would leave the peer's direction
	// open, and bytes still arriving for a client that is gone would never be
	// consumed.
	bridgeAbort(peer, err)
}

// peerGone reports a peer the bridge can no longer deliver to: it closed or
// is closing. A forward that fails because of that says the pair is being
// torn down, not that the reading leg erred — failing the leg there would
// reset a half-close chain that is ending cleanly on both ends.
func peerGone(peer uio.Conn, err error) bool {
	return errors.Is(err, net.ErrClosed) || bridgeConnGone(peer)
}

// bridgeConnGone reports a closing or closed connection from any goroutine.
// The lifecycle check of a built-in connection reads no callback-scoped
// state, as State's end-of-stream bit does; a custom Conn has only State.
func bridgeConnGone(c uio.Conn) bool {
	if control := readcontrol.ControlOf(resolveControlBase(c)); control != nil {
		return errors.Is(control.Check(), net.ErrClosed)
	}
	return c.State()&(uio.StateClosing|uio.StateClosed) != 0
}

// OnBytesRead forwards transport reads when an Observer is set.
func (b *Bridge) OnBytesRead(c uio.Conn, n int) {
	if b.Observer != nil {
		b.Observer.OnBytesRead(c, n)
	}
}

// OnBytesWritten lets a leg waiting on this one's queue go on, then reports.
func (b *Bridge) OnBytesWritten(c uio.Conn, n int) {
	b.resumeSource(c)
	if b.Observer != nil {
		b.Observer.OnBytesWritten(c, n)
	}
}

// OnStart records the Events when the application wired one by hand, then
// runs the configured hooks.
func (b *Bridge) OnStart(ev *uio.Events) {
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
func (b *Bridge) OnStop(ev *uio.Events) {
	if b.Hooks != nil {
		b.Hooks.OnStop(ev)
	}
}

var (
	_ uio.Handler         = (*Bridge)(nil)
	_ uio.TrafficObserver = (*Bridge)(nil)
	_ uio.ServeHooks      = (*Bridge)(nil)
)
