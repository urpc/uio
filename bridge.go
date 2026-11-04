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
	"sync/atomic"

	"github.com/urpc/uio/internal/readcontrol"
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
	conn Conn
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
func (p *bridgePair) legs(c Conn) (own, other *bridgeLeg) {
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
//	br := &uio.Bridge{Upstream: "127.0.0.1:9527"}
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
// The application's PauseReads switch is independent of it.
type Bridge struct {
	// Upstream is the address every client leg is relayed to, dialed
	// through the bridge's own Events.
	Upstream string
	// Dial replaces the Upstream dial: it receives the client leg and a dial
	// function already bound to the bridge's Events, and returns the upstream
	// leg, which the bridge pairs with the client once Dial returns. It may
	// use the dial function once; a connection it dialed but did not return
	// is closed. An error resets the client leg.
	Dial func(ctx context.Context, client Conn, dial DialFunc) (Conn, error)
	// OnPair is told each finished pairing.
	OnPair func(client, upstream Conn)
	// Observer, when set, hears the byte counts the bridge's legs report.
	Observer TrafficObserver
	// Hooks, when set, runs around the bridge's Events' Serve.
	Hooks ServeHooks
	// InboundLimit bounds the bytes a leg leaves unconsumed and is the high
	// watermark of its destination's queue (default 256 KiB). It also
	// becomes the Events' MaxInboundBuffered.
	InboundLimit int

	mu     sync.Mutex
	events *Events
	pairs  map[Conn]*bridgePair
}

// DialFunc dials through the bridge's Events; the bridge adds its own mark
// to opts.
type DialFunc func(ctx context.Context, addr string, opts ...DialOption) (Conn, error)

var (
	// errBridgePeerGone closes a leg whose partner was already closed when
	// the bridge tried to pair them.
	errBridgePeerGone = errors.New("uio: paired connection already closed")
	// errBridgePaired refuses a pairing with a connection already relayed.
	errBridgePaired = errors.New("uio: connection already belongs to another bridge pair")
)

// Pair links two connections the bridge already receives the callbacks of:
// both must be handled by this bridge's Events or by a umux session whose
// Handler is this bridge. A Dial override's returned connection is paired
// this way by the bridge itself. Repeating a pairing is harmless; a
// connection already paired with another is refused with errBridgePaired.
// On failure the caller remains responsible for closing its connections.
func (b *Bridge) Pair(a, c Conn) error {
	if a == nil || c == nil || a == c {
		return ErrUnsupported
	}
	p := &bridgePair{
		a: bridgeLeg{conn: a, pause: readcontrol.NewPause(a)},
		b: bridgeLeg{conn: c, pause: readcontrol.NewPause(c)},
	}
	b.mu.Lock()
	if b.pairs == nil {
		b.pairs = make(map[Conn]*bridgePair)
	}
	if old := b.pairs[a]; old != nil || b.pairs[c] != nil {
		same := old != nil && old == b.pairs[c] && !old.closed.Load()
		b.mu.Unlock()
		if same {
			return nil
		}
		return errBridgePaired
	}
	b.pairs[a], b.pairs[c] = p, p
	b.mu.Unlock()
	// A close that completed before the pair was published could not find
	// it, and its OnClose does not repeat: look again now that it is.
	if bridgeConnGone(a) || bridgeConnGone(c) || p.closed.Load() {
		p.closed.Store(true)
		b.mu.Lock()
		delete(b.pairs, a)
		delete(b.pairs, c)
		b.mu.Unlock()
		return errBridgePeerGone
	}
	if b.OnPair != nil {
		b.OnPair(a, c)
	}
	// Bytes either leg received before the pairing wait for a turn.
	_ = a.Wake()
	_ = c.Wake()
	return nil
}

func (b *Bridge) pairOf(c Conn) *bridgePair {
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
func (b *Bridge) Events() *Events {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.events == nil {
		b.events = &Events{
			Handler:            b,
			Observer:           b,
			Hooks:              b,
			AllowHalfOpen:      true,
			MaxBufferSize:      64 << 10,
			MaxInboundBuffered: b.inboundLimit(),
		}
	}
	return b.events
}

// bridgeAbort resets a leg the relay cannot carry on.
func bridgeAbort(c Conn, err error) {
	if c != nil {
		_ = c.SetLinger(0)
		_ = c.CloseWith(err)
	}
}

// OnOpen dials the upstream of a new client leg and pairs the two once the
// dial returned. An upstream the dial helper made waits for that pairing.
// It runs on the connection's turn, so a synchronous Dial delays that turn
// until the upstream is up.
func (b *Bridge) OnOpen(c Conn) {
	if _, upstream := c.Userdata().(bridgePeerMark); upstream {
		return
	}
	if b.pairOf(c) != nil {
		// Paired before its OnOpen ran — a stream a Dial override returned,
		// or a Pair call: bytes it held flow now.
		_ = c.Wake()
		return
	}
	var helper Conn
	dial := func(ctx context.Context, addr string, opts ...DialOption) (Conn, error) {
		opts = append(opts, WithUserdata(bridgePeerMark{}))
		up, err := b.Events().DialContext(ctx, addr, opts...)
		helper = up
		return up, err
	}
	var up Conn
	var err error
	switch {
	case b.Dial != nil:
		up, err = b.Dial(context.Background(), c, dial)
	case b.Upstream != "":
		up, err = dial(context.Background(), b.Upstream)
	default:
		err = errors.New("uio: bridge has no upstream")
	}
	if err == nil {
		err = b.Pair(c, up)
	}
	if err != nil {
		if b.pairOf(c) == nil {
			bridgeAbort(c, err)
		} // else the override paired it elsewhere: that pair owns it now.
		if up != c && (!errors.Is(err, errBridgePaired) || b.pairOf(up) == nil) {
			bridgeAbort(up, err)
		}
		if helper != c && helper != up && b.pairOf(helper) == nil {
			bridgeAbort(helper, err)
		}
		return
	}
	if helper != nil && helper != up {
		bridgeAbort(helper, errBridgePeerGone) // dialed, then not returned
	}
}

// OnRead forwards a leg's input to its destination while that destination's
// queue is below InboundLimit, and passes the end of the stream on.
// Otherwise the input stays unconsumed until the destination drains to half
// the limit; see resumeSource.
func (b *Bridge) OnRead(c Conn) error {
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
		if c.State()&StatePeerEnded != 0 {
			// Graceful, not Close: queued bytes of this leg must still reach
			// the wire before it ends.
			_ = c.CloseGracefully(nil)
		}
		return nil
	}
	own, other := p.legs(c)
	peer := other.conn
	high := b.inboundLimit()
	drains := own.drains.Load()
	if peer.OutboundBuffered() < high {
		n, err := c.WriteTo(peer)
		if n == 0 && errors.Is(err, ErrOutboundOverflow) && peer.OutboundBuffered() == 0 {
			// A chunk larger than the empty destination's whole budget:
			// forward a prefix it takes, the rest on its drains.
			_, err = c.WriteTo(bridgePrefixWriter{peer})
		}
		if err != nil && !errors.Is(err, ErrOutboundOverflow) {
			if peerGone(peer, err) {
				return nil // the pair is being torn down; its close releases this leg
			}
			return err
		}
	}
	if c.InboundBuffered() == 0 {
		// StatePeerEnded means the peer ended and everything it sent was
		// read: the end follows the bytes.
		if c.State()&StatePeerEnded != 0 {
			if err := peer.CloseWrite(); err != nil && !peerGone(peer, err) {
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
func (b *Bridge) resumeSource(destination Conn) {
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
type bridgePrefixWriter struct{ destination Conn }

func (w bridgePrefixWriter) Write(p []byte) (int, error) {
	want := len(p)
	for {
		n, err := w.destination.Write(p)
		if n != 0 || !errors.Is(err, ErrOutboundOverflow) || len(p) <= 1 || w.destination.OutboundBuffered() != 0 {
			if err == nil && len(p) < want {
				err = ErrOutboundOverflow
			}
			return n, err
		}
		p = p[:len(p)/2]
	}
}

// OnTimer is a no-op: the bridge keeps no timers of its own.
func (b *Bridge) OnTimer(Conn) error { return nil }

// OnClose passes an abort on as an abort and a clean end as the end of the
// partner's writing half; the partner then discards what still arrives for
// the gone leg and closes once it ended too.
func (b *Bridge) OnClose(c Conn, err error) {
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
		_ = peer.CloseWrite()
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
	// CloseWith(err) aborts: a graceful Close would leave the peer's
	// direction open, and bytes still arriving for a client that is gone
	// would never be consumed.
	bridgeAbort(peer, err)
}

// peerGone reports a peer the bridge can no longer deliver to: it closed or
// is closing. A forward that fails because of that says the pair is being
// torn down, not that the reading leg erred — failing the leg there would
// reset a half-close chain that is ending cleanly on both ends.
func peerGone(peer Conn, err error) bool {
	return errors.Is(err, net.ErrClosed) || bridgeConnGone(peer)
}

// bridgeConnGone reports a closing or closed connection from any goroutine.
// The lifecycle check of a built-in connection reads no callback-scoped
// state, as State's end-of-stream bit does; a custom Conn has only State.
func bridgeConnGone(c Conn) bool {
	if control := readcontrol.ControlOf(c); control != nil {
		return errors.Is(control.Check(), net.ErrClosed)
	}
	return c.State()&(StateClosing|StateClosed) != 0
}

// OnQueueDrained hears a connection whose queue drained without a write
// report — a umux stream's queue its transport took, output a layer held
// and its peer acknowledged; a leg waiting on that queue can go on.
func (b *Bridge) OnQueueDrained(c Conn) { b.resumeSource(c) }

// OnBytesRead forwards transport reads when an Observer is set.
func (b *Bridge) OnBytesRead(c Conn, n int) {
	if b.Observer != nil {
		b.Observer.OnBytesRead(c, n)
	}
}

// OnBytesWritten lets a leg waiting on this one's queue go on, then reports.
func (b *Bridge) OnBytesWritten(c Conn, n int) {
	b.resumeSource(c)
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
