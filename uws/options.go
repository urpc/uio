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
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/urpc/uio"
)

// DefaultMaxInboundBuffered is the default transport receive bound, so an
// explicit Conn.PauseRead stops socket reads once buffered input reaches it.
const DefaultMaxInboundBuffered = 64 << 10

// ErrInvalidOptions reports Options a server or dialer cannot run with.
var ErrInvalidOptions = errors.New("uws: invalid options")

// Options holds every setting a Server or Dialer accepts. The zero value
// means the defaults; the fields are documented with them. Server and Dialer
// share the type so the two ends cannot drift apart. A value is read when
// Serve or Dial first starts its transport and never again: changing the
// Options of a running Server or Dialer has no effect.
type Options struct {
	// Handler receives the connection callbacks of every connection the
	// server or dialer makes. It may be nil, which leaves the protocol
	// running without application callbacks; a Dialer whose Handler is set
	// refuses DialClient, which installs its own.
	Handler Handler
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
	// disables heartbeats; both servers and dialers may enable them. The
	// heartbeat runs in the connection's own turns, so a Handler callback
	// that blocks also pauses its connection's heartbeat until it returns;
	// a paused BlockingConn / Client never blocks turns, so its heartbeat
	// timeout continues to run natively.
	HeartbeatInterval time.Duration
	// HeartbeatTimeout bounds both the time a ping may remain queued and the
	// time to receive its matching pong after it is written. Zero uses twice
	// HeartbeatInterval.
	HeartbeatTimeout time.Duration

	// Pollers is the number of event loops. Zero uses UIO's default.
	Pollers int
	// ReadBufferSize is the size of each socket read. Zero uses UIO's default
	// of 4 KiB. It also sizes the write batch blocks the connection's replies
	// are coalesced into, clamped to 4-64 KiB. With TLSConfig a record
	// carries up to 16 KiB and a read smaller than that splits one record
	// across several rounds; a wss server moving bulk data reads about twice
	// as fast with 16 KiB and half again with 64 KiB.
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

	// TLSConfig makes the connections wss. On a Server it holds the
	// certificates, and every connection the Server accepts runs TLS under
	// the WebSocket protocol; ServeHTTP still takes plain HTTP/1.1 upgrades
	// only. Authorize sees the TLS state on its request's TLS field, and
	// Conn.TLSState reports it. On a Dialer it is what wss:// dials verify
	// the server with — an empty Config verifies against the system roots —
	// and when it names no ServerName, each dial verifies the host its URL
	// names; ws:// dials run without TLS. A Dialer without it refuses wss://.
	// The TLS handshake runs before the WebSocket one, each bounded by
	// HandshakeTimeout, and a dial that fails it reports its cause.
	TLSConfig *tls.Config
	// MaxInboundBuffered bounds unread input buffered in the transport, which
	// lets an explicit Conn.PauseRead apply TCP backpressure to the peer. Zero
	// uses DefaultMaxInboundBuffered (64 KiB); a negative value disables the
	// explicit bound beyond UIO's pause floor.
	MaxInboundBuffered int
	// MaxOutboundBuffered bounds the bytes queued for one connection from any
	// sender: a send that would pass it returns ErrBackpressure and queues
	// nothing, so a slow peer cannot grow the queue without bound however the
	// output is produced. Size it to fit the largest message sent. Zero means
	// no limit.
	MaxOutboundBuffered int
}

// TrafficObserver reports transport traffic. Implement it on the Handler to
// receive both directions of every connection.
//
// Both callbacks are observation only. They must not call any method that
// sends or closes on the connection (SendText, SendBinary, Ping, Close, ...):
// a report is the result of I/O that just happened, and writing from it feeds
// the report it answers — it retires more bytes, which reports again. In uws
// the report can also run while the write lock a send holds is still held, so
// a write here deadlocks the connection. They must not block either.
// OutboundBuffered, PauseRead and ResumeRead are safe here: they do not
// acquire the write lock or write to the transport.
//
// OnBytesRead runs in the connection's turn and never overlaps its Handler
// callbacks. OnBytesWritten runs wherever the transport finishes a send — a
// write turn, a writer goroutine, or the goroutine closing the connection,
// possibly with internal locks held — and may overlap the connection's Handler
// callbacks and OnBytesRead, so state it shares with them must be safe for
// concurrent use. Reports made inside a WebSocket batch handoff may be
// combined and delivered just after handoff, preserving their byte total.
type TrafficObserver interface {
	OnBytesRead(*Conn, int)
	OnBytesWritten(*Conn, int)
}

// implementsTrafficObserver reports whether h wants per-connection traffic
// reports.
func implementsTrafficObserver(h Handler) bool {
	_, ok := h.(TrafficObserver)
	return ok
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
	if r.MaxInboundBuffered == 0 {
		r.MaxInboundBuffered = DefaultMaxInboundBuffered
	} else if r.MaxInboundBuffered < 0 {
		r.MaxInboundBuffered = 0
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
	case r.MaxOutboundBuffered < 0:
		return invalid("MaxOutboundBuffered %d is negative", r.MaxOutboundBuffered)
	case r.EnableCompression && r.CompressionLevel != 0 && (r.CompressionLevel < 1 || r.CompressionLevel > 9):
		return invalid("CompressionLevel %d is outside 1..9", r.CompressionLevel)
	case !r.EnableCompression && r.CompressionLevel != 0:
		return invalid("CompressionLevel %d is outside 1..9 while compression is disabled", r.CompressionLevel)
	}
	return nil
}

// transportEvents builds the private UIO transport configuration. The Events
// value belongs to the Server or Dialer after this call; nothing else may
// keep or mutate it.
func (o Options) transportEvents() *uio.Events {
	inboundLimit := o.MaxInboundBuffered
	if inboundLimit <= 0 {
		inboundLimit = DefaultMaxInboundBuffered
	}
	return &uio.Events{
		Pollers:                o.Pollers,
		ReadBufferSize:         o.ReadBufferSize,
		WriteBufferedThreshold: o.WriteCoalesce,
		ReusePort:              o.ReusePort,
		Executor:               o.Executor,
		MaxOutboundBuffered:    o.MaxOutboundBuffered,
		MaxInboundBuffered:     inboundLimit,
	}
}

// framesPerDataRead bounds the frames one read round consumes: a full read
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
		readSize = 4 << 10 // UIO's ReadBufferSize default
	}
	return min(max(readSize, minWriteBatchBlock), maxWriteBatchBlock)
}
