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

package uws

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/urpc/uio"
)

// DefaultOutboundHighWatermark is the default queued-output level at which a
// connection stops reading its socket.
const DefaultOutboundHighWatermark = 64 << 10

// ErrInvalidOptions reports Options a server or dialer cannot run with.
var ErrInvalidOptions = errors.New("uws: invalid options")

// Options holds every setting a Server or Dialer accepts. The zero value
// means the defaults; the fields are documented with them. Server and Dialer
// share the type so the two ends cannot drift apart. A value is read when
// Serve or Dial first starts its transport and never again: changing the
// Options of a running Server or Dialer has no effect.
type Options struct {
	// Subprotocols lists supported protocols in server preference order,
	// offered in the same order by a Dialer.
	Subprotocols []string

	// MaxHeaderBytes bounds the HTTP upgrade headers this end reads: the
	// request on a server, the response on a dialer. Zero uses 32 KiB.
	MaxHeaderBytes int
	// MaxFramePayload bounds each wire frame payload. On reads, it also
	// bounds aggregate compressed payload across a fragmented message. Zero
	// uses 16 MiB.
	MaxFramePayload uint64
	// MaxMessageSize bounds a complete message after decompression. Zero uses
	// 64 MiB.
	MaxMessageSize uint64

	// EnableCompression enables RFC 7692 permessage-deflate.
	EnableCompression bool
	// CompressionLevel selects the flate level, 1 through 9. Zero uses the
	// library default level; there is no level 0: permessage-deflate with no
	// compression only adds framing overhead.
	CompressionLevel int
	// KeepCompressionContext keeps this end's compression context between
	// messages. Off by default: every message starts a fresh context and the
	// negotiation asks for the same of the peer, which bounds per-connection
	// compression state.
	KeepCompressionContext bool
	// DisableUTF8Check skips Text message UTF-8 validation. This violates RFC
	// 6455 and should only be used with trusted peers. Close reasons remain
	// validated.
	DisableUTF8Check bool

	// HandshakeTimeout bounds the HTTP upgrade handshake. Zero uses 10s.
	HandshakeTimeout time.Duration
	// CloseTimeout bounds the graceful WebSocket close handshake. Zero uses
	// 5s.
	CloseTimeout time.Duration

	// HeartbeatInterval makes this end ping its peer when positive. Zero
	// disables heartbeats; both servers and dialers may enable them.
	HeartbeatInterval time.Duration
	// HeartbeatTimeout bounds both the time a ping may remain queued and the
	// time to receive its matching pong after it is written. Zero uses twice
	// HeartbeatInterval.
	HeartbeatTimeout time.Duration

	// Pollers is the number of event loops. Zero uses UIO's default.
	Pollers int
	// ReadBufferSize is the size of each socket read. Zero uses UIO's default
	// of 4 KiB. It also sizes the write batch blocks the connection's replies
	// are coalesced into, clamped to 4-64 KiB.
	ReadBufferSize int
	// WriteCoalesce makes callback writes smaller than this value join the
	// connection's outbound batch instead of attempting an immediate send.
	// Zero uses 4 KiB; a negative value disables size-based buffering.
	WriteCoalesce int
	// ReusePort sets SO_REUSEPORT on listeners.
	ReusePort bool
	// Executor runs the connection and write turns; nil uses UIO's own
	// scheduler.
	Executor uio.Executor

	// OutboundHighWatermark is the queued output at which a connection stops
	// reading its socket, so TCP flow control slows its peer instead of the
	// replies to its requests piling up. It paces only what the connection's
	// own reads produce; output sent from elsewhere, such as a broadcast, is
	// bounded by MaxOutboundBuffered. A negative value disables the pause.
	// Zero uses 64 KiB.
	OutboundHighWatermark int
	// OutboundLowWatermark is the queued level at or below which a paused
	// connection reads again. Zero uses a quarter of the high watermark.
	OutboundLowWatermark int
	// MaxOutboundBuffered bounds the bytes queued for one connection from any
	// sender: a send that would pass it returns ErrBackpressure and queues
	// nothing, so a slow peer cannot grow the queue without bound however the
	// output is produced. Size it to fit the largest message sent. Zero means
	// no limit.
	MaxOutboundBuffered int
}

// TrafficObserver reports transport traffic. Implement it on the Handler to
// receive both directions of every connection. OnInbound runs in the
// connection's turn and never overlaps its Handler callbacks. OnOutbound runs
// wherever the transport finishes a send — a write turn, a writer goroutine,
// or the goroutine closing the connection — and may overlap the connection's
// Handler callbacks and OnInbound, so state it shares with them must be safe
// for concurrent use. Neither may block. A Send made from OnOutbound is
// allowed: it never waits for a lock the send that triggered the report
// holds, and the reports it delays this way are delivered coalesced, as one
// call carrying the byte total.
type TrafficObserver interface {
	OnInbound(*Conn, int)
	OnOutbound(*Conn, int)
}

// implementsTrafficObserver reports whether h wants per-connection traffic
// reports.
func implementsTrafficObserver(h Handler) bool {
	_, ok := h.(TrafficObserver)
	return ok
}

// Stats reports per-connection read pausing, the effect of
// OutboundHighWatermark: a pause means the peer is not keeping up with its
// replies.
type Stats struct {
	ReadsPaused  uint64
	ReadsResumed uint64
}

// statsCounters is the atomic state behind Stats, shared by every connection
// of one Server or Dialer.
type statsCounters struct {
	paused  atomic.Uint64
	resumed atomic.Uint64
}

func (c *statsCounters) snapshot() Stats {
	if c == nil {
		return Stats{}
	}
	return Stats{ReadsPaused: c.paused.Load(), ReadsResumed: c.resumed.Load()}
}

// resolve returns a copy of o with the defaults applied and the values
// checked. The receiver is left untouched, so the caller's Options stay as
// configured.
func (o Options) resolve() (Options, error) {
	r := o
	if r.MaxHeaderBytes == 0 {
		r.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	if r.MaxFramePayload == 0 {
		r.MaxFramePayload = DefaultMaxFramePayload
	}
	if r.MaxMessageSize == 0 {
		r.MaxMessageSize = DefaultMaxMessageSize
	}
	if r.HandshakeTimeout == 0 {
		r.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if r.CloseTimeout == 0 {
		r.CloseTimeout = DefaultCloseTimeout
	}
	if r.HeartbeatTimeout == 0 && r.HeartbeatInterval > 0 {
		r.HeartbeatTimeout = r.HeartbeatInterval * 2
	}
	if r.WriteCoalesce == 0 {
		r.WriteCoalesce = defaultWriteBufferedThreshold
	}
	if r.OutboundHighWatermark < 0 {
		r.OutboundHighWatermark = 0
	} else if r.OutboundHighWatermark == 0 {
		r.OutboundHighWatermark = DefaultOutboundHighWatermark
	}
	if r.OutboundLowWatermark == 0 && r.OutboundHighWatermark > 0 {
		r.OutboundLowWatermark = r.OutboundHighWatermark / 4
	}
	r.Subprotocols = append([]string(nil), o.Subprotocols...)

	if err := r.validate(); err != nil {
		return Options{}, err
	}
	return r, nil
}

func (r *Options) validate() error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidOptions, fmt.Sprintf(format, args...))
	}
	switch {
	case r.MaxHeaderBytes < 0:
		return invalid("MaxHeaderBytes %d is negative", r.MaxHeaderBytes)
	case r.HandshakeTimeout < 0:
		return invalid("HandshakeTimeout %v is negative", r.HandshakeTimeout)
	case r.CloseTimeout < 0:
		return invalid("CloseTimeout %v is negative", r.CloseTimeout)
	case r.HeartbeatInterval < 0:
		return invalid("HeartbeatInterval %v is negative", r.HeartbeatInterval)
	case r.HeartbeatTimeout < 0:
		return invalid("HeartbeatTimeout %v is negative", r.HeartbeatTimeout)
	case r.Pollers < 0:
		return invalid("Pollers %d is negative", r.Pollers)
	case r.ReadBufferSize < 0:
		return invalid("ReadBufferSize %d is negative", r.ReadBufferSize)
	case r.OutboundLowWatermark < 0:
		return invalid("OutboundLowWatermark %d is negative", r.OutboundLowWatermark)
	case r.MaxOutboundBuffered < 0:
		return invalid("MaxOutboundBuffered %d is negative", r.MaxOutboundBuffered)
	case r.MaxOutboundBuffered > 0 && r.OutboundHighWatermark > r.MaxOutboundBuffered:
		return invalid("OutboundHighWatermark %d is above MaxOutboundBuffered %d: reads would never pause before sends fail",
			r.OutboundHighWatermark, r.MaxOutboundBuffered)
	case r.OutboundHighWatermark > 0 && r.OutboundLowWatermark >= r.OutboundHighWatermark:
		return invalid("OutboundLowWatermark %d is not below OutboundHighWatermark %d",
			r.OutboundLowWatermark, r.OutboundHighWatermark)
	case r.EnableCompression && r.CompressionLevel != 0 && (r.CompressionLevel < 1 || r.CompressionLevel > 9):
		return invalid("CompressionLevel %d is outside 1..9", r.CompressionLevel)
	case !r.EnableCompression && r.CompressionLevel != 0:
		return invalid("CompressionLevel %d set while compression is disabled", r.CompressionLevel)
	}
	return nil
}

// transportEvents builds the private UIO transport configuration. The Events
// value belongs to the Server or Dialer after this call; nothing else may
// keep or mutate it.
func (o Options) transportEvents() *uio.Events {
	return &uio.Events{
		Pollers:                o.Pollers,
		MaxBufferSize:          o.ReadBufferSize,
		WriteBufferedThreshold: o.WriteCoalesce,
		ReusePort:              o.ReusePort,
		Executor:               o.Executor,
		MaxOutboundBuffered:    o.MaxOutboundBuffered,
		// The read pause of OutboundHighWatermark relies on UIO's inbound
		// hold: stopping consumption past this bound stops socket reads.
		MaxInboundBuffered: o.OutboundHighWatermark,
	}
}

// framesPerDataRead bounds the frames one OnData round consumes: a full read
// of the configured buffer at six bytes per frame (the smallest a masked
// client frame can be), never below the fairness floor.
func (o Options) framesPerDataRead() int {
	readSize := o.ReadBufferSize
	if readSize <= 0 {
		readSize = 4 << 10
	}
	return max(minFramesPerDataEvent, readSize/6+1)
}

// batchBlockSize sizes write batch blocks from the transport's read size.
func (o Options) batchBlockSize() int {
	readSize := o.ReadBufferSize
	if readSize <= 0 {
		readSize = 4 << 10 // UIO's MaxBufferSize default
	}
	return min(max(readSize, minWriteBatchBlock), maxWriteBatchBlock)
}
