package uws

import (
	"net/http"
	"sync"
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
	shortSendLimit   int
	shortSendDirect  int
	shortReadLimit   int
	maxOutbound      int // the transport's MaxOutboundBuffered
	batchBlockSize   int
	closeTimeout     time.Duration
	handshakeTimeout time.Duration

	compressionEnabled         bool
	compressionLevel           int
	compressionContextTakeover bool

	subprotocols []string
	checkOrigin  func(*http.Request) bool
	handler      Handler

	heartbeatConnections *sync.Map
	heartbeatInterval    time.Duration
	heartbeatTimeout     time.Duration
}

// newServerConnConfig resolves defaults and copies mutable slices so every
// accepted connection observes one immutable startup snapshot.
func newServerConnConfig(server *Server) *connConfig {
	maxHeader := server.MaxHeaderBytes
	if maxHeader <= 0 {
		maxHeader = DefaultMaxHeaderBytes
	}
	maxFrame := server.MaxFramePayload
	if maxFrame == 0 {
		maxFrame = DefaultMaxFramePayload
	}
	maxMessage := server.MaxMessageSize
	if maxMessage == 0 {
		maxMessage = DefaultMaxMessageSize
	}
	closeTimeout := server.CloseTimeout
	if closeTimeout <= 0 {
		closeTimeout = DefaultCloseTimeout
	}
	handshakeTimeout := server.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = DefaultHandshakeTimeout
	}
	compressionLevel := server.CompressionLevel
	if server.EnableCompression && compressionLevel == 0 {
		compressionLevel = -1
	}
	heartbeatTimeout := server.HeartbeatTimeout
	if server.HeartbeatInterval > 0 && heartbeatTimeout <= 0 {
		heartbeatTimeout = server.HeartbeatInterval * 2
	}

	config := &connConfig{
		parser: frame.ParserConfig{
			ExpectMask: true, AllowRSV1: server.EnableCompression, MaxFramePayload: maxFrame,
		},
		assembler: frame.AssemblerConfig{
			MaxMessage: maxMessage, MaxCompressedPayload: maxFrame, ValidateUTF8: !server.DisableUTF8Check,
		},
		maxHeaderBytes:             maxHeader,
		writeBufferedThreshold:     effectiveWriteBufferedThreshold(server.Events),
		maxOutbound:                maxOutboundBuffered(server.Events),
		batchBlockSize:             writeBatchBlockSize(server.Events),
		closeTimeout:               closeTimeout,
		handshakeTimeout:           handshakeTimeout,
		compressionEnabled:         server.EnableCompression,
		compressionLevel:           compressionLevel,
		compressionContextTakeover: server.AllowCompressionContextTakeover,
		subprotocols:               append([]string(nil), server.Subprotocols...),
		checkOrigin:                server.CheckOrigin,
		handler:                    server.Handler,
		heartbeatInterval:          server.HeartbeatInterval,
		heartbeatTimeout:           heartbeatTimeout,
	}
	if server.HeartbeatInterval > 0 {
		config.heartbeatConnections = &server.connections
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

// newDialerConnConfig freezes the dialer's protocol and timeout policy for all
// connections created during its single lifecycle.
func newDialerConnConfig(dialer *Dialer) *connConfig {
	maxHeader := dialer.MaxHeaderBytes
	if maxHeader <= 0 {
		maxHeader = DefaultMaxHeaderBytes
	}
	maxFrame := dialer.maxFramePayload()
	maxMessage := dialer.maxMessageSize()
	closeTimeout := dialer.CloseTimeout
	if closeTimeout <= 0 {
		closeTimeout = DefaultCloseTimeout
	}
	handshakeTimeout := dialer.handshakeTimeout()
	compressionLevel := dialer.compressionLevel()

	config := &connConfig{
		client: true,
		parser: frame.ParserConfig{
			AllowRSV1: dialer.EnableCompression, MaxFramePayload: maxFrame,
		},
		assembler: frame.AssemblerConfig{
			MaxMessage: maxMessage, MaxCompressedPayload: maxFrame, ValidateUTF8: !dialer.DisableUTF8Check,
		},
		maxHeaderBytes:         maxHeader,
		writeBufferedThreshold: effectiveWriteBufferedThreshold(dialer.Events),
		maxOutbound:            maxOutboundBuffered(dialer.Events),
		batchBlockSize:         writeBatchBlockSize(dialer.Events),
		closeTimeout:           closeTimeout,
		handshakeTimeout:       handshakeTimeout,
		compressionEnabled:     dialer.EnableCompression,
		compressionLevel:       compressionLevel,
		subprotocols:           append([]string(nil), dialer.Subprotocols...),
	}
	config.settleShortLimits()
	return config
}

func (config *connConfig) clientExtensions() string {
	if config == nil || !config.compressionEnabled {
		return ""
	}
	return "permessage-deflate; client_max_window_bits; server_max_window_bits=15"
}
