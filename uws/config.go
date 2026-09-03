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

	maxHeaderBytes   int
	maxOutboundBytes int
	closeTimeout     time.Duration
	handshakeTimeout time.Duration

	compressionEnabled         bool
	compressionLevel           int
	compressionContextTakeover bool

	subprotocols []string
	checkOrigin  func(*http.Request) bool
	handler      Handler
	executor     Executor

	dispatchBudget       *pendingBudget
	heartbeatConnections *sync.Map
	heartbeatInterval    time.Duration
	heartbeatTimeout     time.Duration
}

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
	maxOutbound := server.MaxOutboundBytes
	if maxOutbound == 0 {
		maxOutbound = DefaultMaxOutboundBytes
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
		maxOutboundBytes:           maxOutbound,
		closeTimeout:               closeTimeout,
		handshakeTimeout:           handshakeTimeout,
		compressionEnabled:         server.EnableCompression,
		compressionLevel:           compressionLevel,
		compressionContextTakeover: server.AllowCompressionContextTakeover,
		subprotocols:               append([]string(nil), server.Subprotocols...),
		checkOrigin:                server.CheckOrigin,
		handler:                    server.Handler,
		executor:                   server.Executor,
		dispatchBudget:             &server.dispatchBudget,
		heartbeatInterval:          server.HeartbeatInterval,
		heartbeatTimeout:           heartbeatTimeout,
	}
	if server.HeartbeatInterval > 0 {
		config.heartbeatConnections = &server.connections
	}
	return config
}

func newDialerConnConfig(dialer *Dialer) *connConfig {
	maxHeader := dialer.MaxHeaderBytes
	if maxHeader <= 0 {
		maxHeader = DefaultMaxHeaderBytes
	}
	maxFrame := dialer.maxFramePayload()
	maxMessage := dialer.maxMessageSize()
	maxOutbound := dialer.MaxOutboundBytes
	if maxOutbound == 0 {
		maxOutbound = DefaultMaxOutboundBytes
	}
	closeTimeout := dialer.CloseTimeout
	if closeTimeout <= 0 {
		closeTimeout = DefaultCloseTimeout
	}
	handshakeTimeout := dialer.handshakeTimeout()
	compressionLevel := dialer.compressionLevel()

	return &connConfig{
		client: true,
		parser: frame.ParserConfig{
			AllowRSV1: dialer.EnableCompression, MaxFramePayload: maxFrame,
		},
		assembler: frame.AssemblerConfig{
			MaxMessage: maxMessage, MaxCompressedPayload: maxFrame, ValidateUTF8: !dialer.DisableUTF8Check,
		},
		maxHeaderBytes:     maxHeader,
		maxOutboundBytes:   maxOutbound,
		closeTimeout:       closeTimeout,
		handshakeTimeout:   handshakeTimeout,
		compressionEnabled: dialer.EnableCompression,
		compressionLevel:   compressionLevel,
		subprotocols:       append([]string(nil), dialer.Subprotocols...),
		executor:           dialer.Executor,
		dispatchBudget:     &dialer.dispatchBudget,
	}
}

func (config *connConfig) clientExtensions() string {
	if config == nil || !config.compressionEnabled {
		return ""
	}
	return "permessage-deflate; client_max_window_bits; server_max_window_bits=15"
}
