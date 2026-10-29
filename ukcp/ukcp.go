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

// Package ukcp runs KCP — a reliable, ordered byte stream over an unreliable
// datagram transport — as a UIO layer, with the protocol engine vendored from
// github.com/xtaci/kcp-go (see internal/kcp).
//
// ServerLayer and ClientLayer return a [uio.Layer], so KCP composes with any
// other layer: it takes a packet connection's datagrams (it is a
// [uio.PacketLayer]) or a stream connection's bytes, and hands the layers
// above one reliable stream — TLS and the like can run over it:
//
//	events.Listen("udp://:9000", uio.WithLayers(ukcp.ServerLayer(opts), utls.Server(cfg)))
//	events.Dial("udp://host:9000", uio.WithLayers(ukcp.ClientLayer(opts), utls.Client(cfg)))
//
// Nothing costs a goroutine: every session runs on the connection's turns,
// its clock is the layer's timer, and an idle session wakes for nothing.
//
// A KCP layer above another one is a stream above a stream: the segment
// framing carries it, so nesting composes like any other stream layer. A
// stream connection below a KCP layer is framed by the engine's headers: one
// that ends at a segment boundary ends this stream too, and one that ends
// inside a segment aborts it with io.ErrUnexpectedEOF. The stream dialect
// sends frg=0 only; a segment in message mode ends the session with
// ErrProtocol, since its sequence number could never be delivered in order.
//
// A standalone server and dialer wrap the layers: NewServer's Serve listens
// on udp:// (or tcp://) addresses with Options.Handler receiving each
// connection's callbacks, and Dialer.Dial — DialContext, when the dial needs a
// bound; the package Dial uses DefaultDialer — returns the connection whose
// events reach Options.Handler the same way. There is no built-in TLS: KCP has no such
// standard, and what rides it is the wiring's to choose — put utls.Server or
// utls.Client above ServerLayer/ClientLayer in a WithLayers chain, exactly as
// over TCP. Both standalone endpoints need a Handler: reads arrive through
// its OnRead, as they do for any uio connection.
//
// A session's stream ends in order: CloseWrite queues an empty push after the
// data written before it, a marker the peer reads back as the end of the
// stream (io.EOF at the Handler, or the end of the layer above). A graceful
// whole-connection close sends the same marker best-effort — the connection
// closes below it, so its retransmissions end there; for a reliable end, end
// the writing half with CloseWrite first. A peer that goes silent is found
// out by KeepAlive (WASK probes) and by the engine's dead-link bound.
//
// MTU must not exceed the Events' MaxBufferSize (4 KiB by default), or the
// socket truncates datagrams and segments never reassemble; the defaults
// (1400) fit.
package ukcp

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urpc/uio"
)

const (
	// defaultMTU is the segment size a session starts with, header included:
	// the usual safe UDP payload for the open internet.
	defaultMTU = 1400
	// maxMTU is the largest segment the vendored engine's buffers hold.
	maxMTU = 1500
	// maxSegmentPayload is the most payload one segment can carry: what the
	// engine's receive buffer pool holds, header taken out. A segment that
	// claims more is refused rather than handed to the engine, which would
	// slice past its pool.
	maxSegmentPayload = maxMTU - kcpOverhead

	// defaultWindow is the send and the receive window, in segments.
	defaultWindow = 1024
	// defaultInterval is the Fast profile's flush cadence; the engine clamps
	// it to 10..5000ms.
	defaultInterval = 30 * time.Millisecond
	// defaultResend is the Fast profile's fast-retransmit threshold.
	defaultResend = 2
	// defaultDeadLink is the engine's bound: a segment retransmitted this
	// many times marks the link dead.
	defaultDeadLink = 20
	// defaultHandshakeTimeout bounds a server's wait for the peer's first
	// segment when HandshakeTimeout is zero.
	defaultHandshakeTimeout = 10 * time.Second
	// maxWindow is the largest window KCP's header can carry: the field is a
	// uint16, and a window past it encodes as zero.
	maxWindow = 1<<16 - 1
	// defaultKeepAlive is how long a session may stay silent before it probes,
	// and a third of how long before it gives up.
	defaultKeepAlive = 10 * time.Second
	// defaultMaxBuffered bounds the bytes a session has taken from the writer
	// but not yet handed to the socket.
	defaultMaxBuffered = 4 << 20
)

// DefaultSocketBuffer is the kernel socket buffer the standalone endpoints
// size UDP sockets with when SocketReadBuffer or SocketWriteBuffer is zero:
// a window's burst, with room to spare.
const DefaultSocketBuffer = 4 << 20

// kcpOverhead is the KCP segment header's size; the engine exports the same
// as IKCP_OVERHEAD.
const kcpOverhead = 24

// ErrInvalidOptions reports options no session can run with; every connection
// through such a layer ends with it.
var ErrInvalidOptions = errors.New("ukcp: invalid options")

// ErrDeadLink reports a session whose peer stopped acknowledging: a segment
// was retransmitted the DeadLink bound times.
var ErrDeadLink = errors.New("ukcp: link dead")

// ErrKeepAliveTimeout reports a session whose peer sent nothing back for
// three keepalive periods.
var ErrKeepAliveTimeout = errors.New("ukcp: keepalive timeout")

// ErrProtocol reports a peer speaking outside the stream dialect this layer
// runs — a segment in KCP's message mode, whose sequence number could never
// be delivered in order. The session ends with it.
var ErrProtocol = errors.New("ukcp: protocol violation")

// ErrPeerSilent reports a server-side connection that heard no valid KCP
// segment within HandshakeTimeout: KCP has no handshake, but a peer that
// never speaks must not hold the connection for ever.
var ErrPeerSilent = errors.New("ukcp: no KCP segment arrived from the peer")

// errMessageTooLarge ends a session over a message its receive scratch cannot
// hold; the stream dialect should make it unreachable.
var errMessageTooLarge = errors.New("ukcp: a message larger than the receive buffer")

// errSegmentTooLarge ends a session whose stream lower delivered a segment
// larger than the engine can hold.
var errSegmentTooLarge = errors.New("ukcp: segment larger than the engine can hold")

// Options configures ServerLayer and ClientLayer — and, through them, the
// standalone server and dialer — with the engine's parameters and the
// transport's. The zero value runs the Fast profile {false, 30ms, 2, 1}.
type Options struct {
	// Handler receives the connection callbacks of the standalone server and
	// dialer. A layer hands its stream to the layer above instead; leaving
	// Handler set there is ignored.
	Handler uio.Handler

	// MTU is the largest segment, header included. The engine's buffers hold
	// 1500, so 24..1500; the default is 1400. It must not exceed the Events'
	// MaxBufferSize (4 KiB by default).
	MTU int

	// SendWindow and RecvWindow are the windows, in segments: how much a
	// session may have in flight and how much it buffers for the reader.
	// Zero takes the default of 1024. A window times the MTU bounds what a
	// session holds — 1024 segments at 1400 is about 1.4 MiB each way.
	SendWindow int
	RecvWindow int

	// NoDelay runs the engine's low-latency RTO mode (30ms floor instead of
	// 100ms). The default, part of the Fast profile, is false.
	NoDelay bool

	// Interval is the flush cadence: the engine's clock for retransmissions
	// and window probes. Data never waits for it — a write and every read
	// flush at once — so it only paces the engine's background work. The
	// engine clamps it to 10ms..5s; zero takes the Fast profile's 30ms.
	Interval time.Duration

	// Resend is the fast-retransmit threshold: how many later segments must
	// be acknowledged before a segment is resent without waiting for its
	// timeout. Zero takes the Fast profile's 2; a negative value disables
	// fast retransmit.
	Resend int

	// CongestionControl enables the engine's congestion window (kcp-go's
	// nocwnd=0). It is off by default, as every KCP profile runs it: flow
	// control is the windows' and the retransmission timer's. The engine
	// starts a congestion window at zero and opens it one segment per
	// acknowledgement, so enabling it costs throughput on a healthy link and
	// buys gentler retransmission on a badly congested one.
	CongestionControl bool

	// DeadLink is how many retransmissions of one segment mark the link dead;
	// the session then ends with ErrDeadLink. Zero takes the default of 20.
	DeadLink int

	// HandshakeTimeout bounds a server's wait for the peer's first segment.
	// KCP has no handshake, but a peer that never speaks must not hold a
	// connection for ever. Zero takes the default of 10s; a negative value
	// waits without a bound. A client never waits: its stack opens at once.
	HandshakeTimeout time.Duration

	// KeepAlive is how long a session may hear nothing from its peer before
	// sending a window-probe (WASK) ping; a peer silent for three periods
	// ends the session with ErrKeepAliveTimeout. Zero takes the default of
	// 10s; a negative value disables keepalive.
	KeepAlive time.Duration

	// MaxBuffered bounds the bytes a session has taken from its writer and
	// not yet handed to the socket; a write that would pass it returns
	// ErrOutboundOverflow. Zero takes the default of 4 MiB.
	MaxBuffered int

	// ---- The transport, for the standalone server and dialer ----
	// A layer takes these from its Events instead.

	// MaxBufferSize is the Events' MaxBufferSize, the socket read size. It
	// must be at least MTU; zero leaves the Events default (4 KiB).
	MaxBufferSize int
	// WriteBufferedThreshold is the Events' WriteBufferedThreshold.
	WriteBufferedThreshold int
	// MaxOutboundBuffered is the Events' MaxOutboundBuffered.
	MaxOutboundBuffered int
	// AllowHalfOpen keeps a session open after its peer ended its stream.
	AllowHalfOpen bool
	// Pollers is the Events' event-loop count.
	Pollers int
	// ReusePort shares the bound address.
	ReusePort bool
	// Executor schedules the Events' connection turns.
	Executor uio.Executor

	// SocketReadBuffer and SocketWriteBuffer size the UDP socket's kernel
	// buffers: a KCP session bursts a whole window — about 1.4 MiB at the
	// defaults — and a kernel default of a few hundred KiB drops much of it.
	// Zero takes DefaultSocketBuffer; a negative value leaves the socket's
	// buffers as they came. A layer endpoint sizes its own sockets through
	// uio.WithListenControl and uio.WithDialControl.
	SocketReadBuffer  int
	SocketWriteBuffer int
}

// Validate reports what a layer built from o would refuse.
func (o Options) Validate() error {
	_, err := o.resolve()
	return err
}

// resolve applies the defaults and refuses what no session can run with.
func (o Options) resolve() (Options, error) {
	switch {
	case o.MTU < 0:
		return Options{}, fmt.Errorf("%w: MTU %d", ErrInvalidOptions, o.MTU)
	case o.MTU == 0:
		o.MTU = defaultMTU
	case o.MTU <= kcpOverhead:
		return Options{}, fmt.Errorf("%w: MTU %d leaves no room for a payload", ErrInvalidOptions, o.MTU)
	case o.MTU > maxMTU:
		return Options{}, fmt.Errorf("%w: MTU %d above the engine's %d", ErrInvalidOptions, o.MTU, maxMTU)
	}
	switch {
	case o.SendWindow < 0, o.RecvWindow < 0:
		return Options{}, fmt.Errorf("%w: windows %d/%d", ErrInvalidOptions, o.SendWindow, o.RecvWindow)
	}
	if o.SendWindow == 0 {
		o.SendWindow = defaultWindow
	}
	if o.RecvWindow == 0 {
		o.RecvWindow = defaultWindow
	}
	if o.SendWindow > maxWindow || o.RecvWindow > maxWindow {
		// The header's window is a uint16: a larger one would travel as zero,
		// and the peer would stop sending.
		return Options{}, fmt.Errorf("%w: windows %d/%d above the wire's %d", ErrInvalidOptions, o.SendWindow, o.RecvWindow, maxWindow)
	}
	switch {
	case o.Interval < 0:
		return Options{}, fmt.Errorf("%w: Interval %v", ErrInvalidOptions, o.Interval)
	case o.Interval == 0:
		o.Interval = defaultInterval
	case o.Interval < 10*time.Millisecond, o.Interval > 5*time.Second:
		// The engine would clamp, silently; refuse instead of running
		// something other than what was asked for.
		return Options{}, fmt.Errorf("%w: Interval %v outside 10ms..5s", ErrInvalidOptions, o.Interval)
	}
	switch {
	case o.Resend < 0:
		o.Resend = 0 // fast retransmit off
	case o.Resend == 0:
		o.Resend = defaultResend
	}
	switch {
	case o.DeadLink < 0:
		return Options{}, fmt.Errorf("%w: DeadLink %d", ErrInvalidOptions, o.DeadLink)
	case o.DeadLink == 0:
		o.DeadLink = defaultDeadLink
	}
	switch {
	case o.HandshakeTimeout < 0:
		o.HandshakeTimeout = 0 // no bound
	case o.HandshakeTimeout == 0:
		o.HandshakeTimeout = defaultHandshakeTimeout
	}
	switch {
	case o.KeepAlive < 0:
		o.KeepAlive = 0 // off
	case o.KeepAlive == 0:
		o.KeepAlive = defaultKeepAlive
	}
	switch {
	case o.MaxBuffered < 0:
		return Options{}, fmt.Errorf("%w: MaxBuffered %d", ErrInvalidOptions, o.MaxBuffered)
	case o.MaxBuffered == 0:
		o.MaxBuffered = defaultMaxBuffered
	}
	// A negative socket buffer leaves the kernel's default; zero asks for
	// DefaultSocketBuffer.
	if o.SocketReadBuffer == 0 {
		o.SocketReadBuffer = DefaultSocketBuffer
	}
	if o.SocketWriteBuffer == 0 {
		o.SocketWriteBuffer = DefaultSocketBuffer
	}
	return o, nil
}

// socketControl sizes the sockets the standalone endpoints make; nil when
// both buffers were left alone.
func (o Options) socketControl() uio.SocketControl {
	if o.SocketReadBuffer < 0 && o.SocketWriteBuffer < 0 {
		return nil
	}
	return func(_, _ string, fd uintptr) error {
		if o.SocketReadBuffer > 0 {
			if err := setSocketBuffer(fd, true, o.SocketReadBuffer); err != nil {
				return err
			}
		}
		if o.SocketWriteBuffer > 0 {
			return setSocketBuffer(fd, false, o.SocketWriteBuffer)
		}
		return nil
	}
}

// withKCPScheme gives an address that names no network the one KCP runs on
// commonly: udp.
func withKCPScheme(addr string) string {
	if strings.Contains(addr, "://") {
		return addr
	}
	return "udp://" + addr
}

// checkTransport refuses transport settings no session can run with.
func (o Options) checkTransport() error {
	if o.MaxBufferSize > 0 && o.MaxBufferSize < o.MTU {
		return fmt.Errorf("%w: MaxBufferSize %d below the %d MTU: datagrams would truncate",
			ErrInvalidOptions, o.MaxBufferSize, o.MTU)
	}
	return nil
}

// ServerLayer runs the server side of KCP as a [uio.Layer], for a chain of
// your own — over a packet connection (UDP and the like, where it reads a
// datagram per turn) or over a stream one (TCP, TLS), which its segments
// self-frame on. It opens the layers above when the peer's first segment
// names the session's conversation id, and hands them one reliable stream.
func ServerLayer(opts Options) uio.Layer { return newLayer(opts, false) }

// ClientLayer runs the client side of KCP as a [uio.Layer]: it opens the
// layers above at once with a conversation id of its own, and the dial does
// not wait for the peer — the first segments retransmit until it answers.
// One layer dials one peer.
func ClientLayer(opts Options) uio.Layer { return newLayer(opts, true) }
