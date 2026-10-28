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
	"net/http"
	"time"

	"github.com/urpc/uio/uws/internal/frame"
)

// connConfig is frozen when a Server or Dialer starts and shared by all of
// that owner's connections.
type connConfig struct {
	client bool

	parser    frame.ParserConfig
	assembler frame.AssemblerConfig

	maxHeaderBytes         int
	writeBufferedThreshold int
	// The limits of the fused short paths, all set by settleShortLimits and
	// -1 where a path is off. shortSendLimit is the largest payload a server
	// sends through the batching send path, under 126 bytes; shortSendDirect
	// the largest sendShortLocked takes on the non-batching path, up to a
	// 16-bit length; both within the message and frame limits and small
	// enough to be buffered. shortReadLimit is the largest seven-bit payload
	// consumeShortFrame takes, within the frame and message limits.
	shortSendLimit  int
	shortSendDirect int
	shortReadLimit  int
	maxOutbound     int // MaxOutboundBuffered
	batchBlockSize  int
	// maxFramesPerDataEvent bounds the frames one OnInbound round consumes: at
	// least minFramesPerDataEvent and always enough for one full read of the
	// configured buffer, so a chunk of tiny frames never leaves a backlog.
	maxFramesPerDataEvent int
	closeTimeout          time.Duration
	handshakeTimeout      time.Duration

	compressionEnabled         bool
	compressionLevel           int
	compressionContextTakeover bool

	subprotocols []string
	checkOrigin  func(*http.Request) bool
	authorize    func(*http.Request) (any, error)
	handler      Handler
	// observeTraffic is set when the handler implements TrafficObserver, so
	// the transport only installs the inbound hook and the outbound observer
	// when something is listening.
	observeTraffic bool

	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration

	// outboundHigh pauses a connection's reads at this queued-output level;
	// outboundLow resumes them. Zero disables the pause.
	outboundHigh int
	outboundLow  int
	stats        *statsCounters
}

// newServerConnConfig freezes the resolved options into the immutable
// per-connection policy a Server's connections share.
func newServerConnConfig(opts Options, checkOrigin func(*http.Request) bool, authorize func(*http.Request) (any, error), stats *statsCounters) *connConfig {
	config := &connConfig{
		parser: frame.ParserConfig{
			ExpectMask: true, AllowRSV1: opts.EnableCompression, MaxFramePayload: opts.MaxFramePayload,
		},
		assembler: frame.AssemblerConfig{
			MaxMessage: opts.MaxMessageSize, MaxCompressedPayload: opts.MaxFramePayload, ValidateUTF8: !opts.DisableUTF8Check,
		},
		maxHeaderBytes:             opts.MaxHeaderBytes,
		writeBufferedThreshold:     opts.WriteCoalesce,
		batchBlockSize:             opts.batchBlockSize(),
		maxFramesPerDataEvent:      opts.framesPerDataRead(),
		closeTimeout:               opts.CloseTimeout,
		handshakeTimeout:           opts.HandshakeTimeout,
		compressionEnabled:         opts.EnableCompression,
		compressionLevel:           compressionLevel(opts),
		compressionContextTakeover: opts.KeepCompressionContext,
		subprotocols:               opts.Subprotocols,
		checkOrigin:                checkOrigin,
		authorize:                  authorize,
		handler:                    opts.Handler,
		observeTraffic:             implementsTrafficObserver(opts.Handler),
		heartbeatInterval:          opts.HeartbeatInterval,
		heartbeatTimeout:           opts.HeartbeatTimeout,
		outboundHigh:               opts.OutboundHighWatermark,
		outboundLow:                opts.OutboundLowWatermark,
		maxOutbound:                opts.MaxOutboundBuffered,
		stats:                      stats,
	}
	config.settleShortLimits()
	return config
}

// settleShortLimits derives the limits of the fused short paths. Both
// constructors call it last, once the fields it reads are set: a client takes
// none of the paths, since it masks what it sends and reads frames unmasked,
// so every limit is -1 there.
func (config *connConfig) settleShortLimits() {
	if config.client {
		config.shortSendLimit, config.shortSendDirect, config.shortReadLimit = -1, -1, -1
		return
	}
	config.shortSendLimit = config.withinMessageLimits(bufferedPayloadLimit(config.writeBufferedThreshold, 125))
	config.shortSendDirect = config.withinMessageLimits(bufferedPayloadLimit(config.writeBufferedThreshold, 0xFFFF))
	config.shortReadLimit = config.withinMessageLimits(125)
}

// bufferedPayloadLimit is the largest unmasked payload of at most maxPayload
// bytes whose frame is smaller than threshold, the size below which the
// transport buffers a write, or -1 when no frame is that small. A payload of
// 126 bytes or more takes a 16-bit length and a header two bytes wider.
func bufferedPayloadLimit(threshold, maxPayload int) int {
	if limit := min(threshold-5, maxPayload); limit >= 126 {
		return limit
	}
	return max(-1, min(threshold-3, 125, maxPayload))
}

// withinMessageLimits lowers limit to the frame and message limits, either of
// which is unlimited at zero, as the parser and assembler read them.
func (config *connConfig) withinMessageLimits(limit int) int {
	if limit < 0 {
		return -1
	}
	for _, bound := range [...]uint64{config.parser.MaxFramePayload, config.assembler.MaxMessage} {
		if bound != 0 && bound < uint64(limit) {
			limit = int(bound)
		}
	}
	return limit
}

// newDialerConnConfig freezes the resolved options into the immutable
// per-connection policy a Dialer's connections share.
func newDialerConnConfig(opts Options, stats *statsCounters) *connConfig {
	config := &connConfig{
		client: true,
		parser: frame.ParserConfig{
			AllowRSV1: opts.EnableCompression, MaxFramePayload: opts.MaxFramePayload,
		},
		assembler: frame.AssemblerConfig{
			MaxMessage: opts.MaxMessageSize, MaxCompressedPayload: opts.MaxFramePayload, ValidateUTF8: !opts.DisableUTF8Check,
		},
		maxHeaderBytes:             opts.MaxHeaderBytes,
		writeBufferedThreshold:     opts.WriteCoalesce,
		batchBlockSize:             opts.batchBlockSize(),
		maxFramesPerDataEvent:      opts.framesPerDataRead(),
		closeTimeout:               opts.CloseTimeout,
		handshakeTimeout:           opts.HandshakeTimeout,
		compressionEnabled:         opts.EnableCompression,
		compressionLevel:           compressionLevel(opts),
		compressionContextTakeover: opts.KeepCompressionContext,
		subprotocols:               opts.Subprotocols,
		handler:                    opts.Handler,
		observeTraffic:             implementsTrafficObserver(opts.Handler),
		heartbeatInterval:          opts.HeartbeatInterval,
		heartbeatTimeout:           opts.HeartbeatTimeout,
		outboundHigh:               opts.OutboundHighWatermark,
		outboundLow:                opts.OutboundLowWatermark,
		maxOutbound:                opts.MaxOutboundBuffered,
		stats:                      stats,
	}
	config.settleShortLimits()
	return config
}

// compressionLevel maps the public level to flate's: zero asks for the
// library default, and a disabled compression a level is never consulted.
func compressionLevel(opts Options) int {
	if !opts.EnableCompression {
		return 0
	}
	if opts.CompressionLevel == 0 {
		return -1 // flate's DefaultCompression
	}
	return opts.CompressionLevel
}

func (config *connConfig) clientExtensions() string {
	if config == nil || !config.compressionEnabled {
		return ""
	}
	// Offer both window parameters so either endpoint may negotiate a smaller
	// RFC 7692 window when it needs to bound compression state. Without a kept
	// context also ask both directions to reset per message, which is what
	// this end does with its own compressor.
	extensions := "permessage-deflate; client_max_window_bits; server_max_window_bits=15"
	if !config.compressionContextTakeover {
		extensions += "; server_no_context_takeover; client_no_context_takeover"
	}
	return extensions
}
