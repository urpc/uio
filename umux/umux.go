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

package umux

import (
	"errors"
	"math"
	"time"

	"github.com/urpc/uio"
)

// Errors a session or stream reports. Stream errors arrive through the
// handler's OnClose; Open and Write return them directly.
var (
	// ErrClosed reports a session or stream already closed.
	ErrClosed = errors.New("umux: closed")
	// ErrStreamReset reports a stream the peer reset with RST.
	ErrStreamReset = errors.New("umux: stream reset")
	// ErrGoAway reports an open attempted after the peer's GoAway.
	ErrGoAway = errors.New("umux: session is going away")
	// ErrShutdownTimeout reports a stream a graceful Shutdown stopped waiting
	// for: it is reset with this cause once the shutdown context ends.
	ErrShutdownTimeout = errors.New("umux: shutdown timeout")
	// ErrMaxStreams reports the peer's stream limit or accept backlog full.
	ErrMaxStreams = errors.New("umux: stream limit reached")
	// ErrProtocol reports a frame the peer must not send.
	ErrProtocol = errors.New("umux: protocol error")
	// ErrStreamOpenTimeout reports an Open whose ACK never arrived.
	ErrStreamOpenTimeout = errors.New("umux: stream open timed out")
	// ErrKeepAlive reports a session that stopped answering pings.
	ErrKeepAlive = errors.New("umux: keepalive timeout")
	// ErrMetadataTooLarge reports OpenWith metadata past Config.MaxMetadata.
	ErrMetadataTooLarge = errors.New("umux: stream metadata exceeds MaxMetadata")
)

// MetadataHandler is an optional capability of Config.Handler. When the
// handler implements it, the session calls OnMetadata just before OnOpen with
// the opaque bytes the opener attached to the stream's SYN through
// Session.OpenWith. The slice is only valid for the duration of the call: the
// session keeps no reference, and a handler that wants it later copies it. A
// handler without the capability never sees metadata. In pull mode the bytes
// travel in the Accepted value instead.
type MetadataHandler interface {
	OnMetadata(c uio.Conn, meta []byte)
}

// Config shapes one Session. Zero values take the defaults shown.
type Config struct {
	// Handler receives every accepted stream's callbacks; its Conn argument
	// is the *Stream. When nil, streams are handed to Session.Accept.
	Handler uio.Handler
	// Observer, when set, is told every stream's read and written bytes on
	// top of the transport's own observation. OnBytesWritten also reports a
	// window update that reopened blocked writers, with n == 0.
	Observer uio.TrafficObserver
	// Accept, when set, vets an inbound stream before it is accepted; a
	// non-nil error rejects it with RST. It sees no metadata: the frame is
	// read for the Stream that follows, so the decision is the handler's (in
	// MetadataHandler.OnMetadata) or the puller's (in the Accepted value).
	Accept func() error

	// ReadBufferSize sizes each transport socket read (default 256 KiB); a data
	// frame spans several reads otherwise, and every read costs a turn.
	ReadBufferSize int
	// WindowSize is each stream's receive window (default 256 KiB, at least
	// 256 KiB). The peer's send window is what its SYN/ACK announces: each
	// side starts at the protocol floor of 256 KiB and the announcement adds
	// the difference, so an asymmetric configuration cannot overrun the
	// other end. It also caps what this end will send in flight to a peer
	// that announced a larger window.
	WindowSize int
	// SessionWindow bounds the bytes buffered across all streams of one
	// session (default 4 MiB); a session buffering more stops reading its
	// transport until consumption brings it back under the budget, so TCP
	// flow control slows the peer instead of memory growing.
	SessionWindow int
	// MaxStreams bounds concurrently open streams (default 1024).
	MaxStreams int
	// MaxFrame bounds a data frame's payload (default 32 KiB, at most 64 KiB).
	MaxFrame int
	// MaxMetadata bounds the opaque bytes a stream's opener may attach to its
	// SYN through Session.OpenWith (default 4 KiB, at most 1 MiB). The
	// extension frame is outside yamux v1; both ends must be umux when it is
	// used. The session holds the bytes only until it hands them over — for
	// the duration of MetadataHandler.OnMetadata, or until the Accepted value
	// leaves the accept queue — so in pull mode retained metadata is bounded
	// by AcceptBacklog × MaxMetadata.
	MaxMetadata int
	// AcceptBacklog bounds the accept queue when Handler is nil (default 256).
	AcceptBacklog int
	// StreamOpenTimeout bounds a SYN awaiting its ACK (default 75s).
	StreamOpenTimeout time.Duration
	// StreamCloseTimeout bounds a half-closed stream before it is reset
	// (default 5m).
	StreamCloseTimeout time.Duration
	// KeepAlive is the ping interval (default 30s); a negative value disables it.
	KeepAlive time.Duration
	// KeepAliveMax bounds the wait for a ping's answer (default 60s).
	KeepAliveMax time.Duration
	// Executor is reserved for per-stream turn scheduling; nil keeps the
	// synchronous demultiplexing of the transport's turn.
	Executor uio.Executor
	// BlockingWrite makes Stream.Write, Writev and WriteOwned suspend calling
	// application goroutines when the peer's stream receive window is exhausted
	// instead of returning uio.ErrOutboundOverflow. Writes made directly on a
	// connection turn remain non-blocking to prevent turn self-deadlock.
	BlockingWrite bool
}

const (
	// minWindow is the yamux v1 initial stream window: every sender starts
	// here, and an endpoint's SYN/ACK announces its receive window as the
	// delta above it.
	minWindow = 256 << 10
	// maxWindowSize bounds WindowSize to what the wire carries: the length
	// field of a data frame and the window delta a SYN announces are uint32,
	// so a larger configured window would truncate at those boundaries and
	// turn into a smaller limit.
	maxWindowSize        = math.MaxUint32
	defaultReadBuffer    = 256 << 10
	defaultWindowSize    = 256 << 10
	defaultSessionWindow = 4 << 20
	defaultMaxStreams    = 1024
	defaultMaxFrame      = 32 << 10
	maxMaxFrame          = 64 << 10
	defaultMaxMetadata   = 4 << 10
	maxMaxMetadata       = 1 << 20
	defaultAcceptBacklog = 256
	defaultStreamOpen    = 75 * time.Second
	defaultStreamClose   = 5 * time.Minute
	defaultKeepAlive     = 30 * time.Second
	defaultKeepAliveMax  = 60 * time.Second
)

// resolve fills every unset field with its default and clamps the frame size.
func (c Config) resolve() Config {
	// The yamux v1 layout starts every sender with a 256 KiB guess, so a
	// smaller receive window would be overrun by a peer's first write.
	if c.ReadBufferSize <= 0 {
		c.ReadBufferSize = defaultReadBuffer
	}
	if c.WindowSize < defaultWindowSize {
		c.WindowSize = defaultWindowSize
	}
	// WindowSize is an int API, while the wire carries a uint32 delta. On
	// 32-bit targets the largest representable int is below the wire limit;
	// compute the effective bound at run time so the package still compiles.
	windowLimit := int64(maxWindowSize)
	if hostLimit := int64(^uint(0) >> 1); hostLimit < windowLimit {
		windowLimit = hostLimit
	}
	if int64(c.WindowSize) > windowLimit {
		c.WindowSize = int(windowLimit)
	}
	if c.SessionWindow <= 0 {
		c.SessionWindow = defaultSessionWindow
	}
	if c.MaxStreams <= 0 {
		c.MaxStreams = defaultMaxStreams
	}
	if c.MaxFrame <= 0 {
		c.MaxFrame = defaultMaxFrame
	}
	if c.MaxFrame > maxMaxFrame {
		c.MaxFrame = maxMaxFrame
	}
	if c.MaxMetadata <= 0 {
		c.MaxMetadata = defaultMaxMetadata
	}
	if c.MaxMetadata > maxMaxMetadata {
		c.MaxMetadata = maxMaxMetadata
	}
	if c.AcceptBacklog <= 0 {
		c.AcceptBacklog = defaultAcceptBacklog
	}
	if c.StreamOpenTimeout <= 0 {
		c.StreamOpenTimeout = defaultStreamOpen
	}
	if c.StreamCloseTimeout <= 0 {
		c.StreamCloseTimeout = defaultStreamClose
	}
	if c.KeepAlive < 0 {
		c.KeepAlive = 0
	} else if c.KeepAlive == 0 {
		c.KeepAlive = defaultKeepAlive
	}
	if c.KeepAliveMax <= 0 {
		c.KeepAliveMax = defaultKeepAliveMax
	}
	return c
}
