package uws

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/extension"
	"github.com/urpc/uio/uws/internal/handshake"
)

var _ http.Handler = (*Server)(nil)

const (
	httpErrorInvalidUpgrade   = "invalid WebSocket upgrade request"
	httpErrorServerNotServing = "server is not serving"
	httpErrorUnsupportedHTTP  = "only plain HTTP/1.1 TCP is supported"
	httpErrorCannotHijack     = "HTTP connection cannot be hijacked"
	httpErrorEarlyData        = "unexpected data before WebSocket upgrade"
)

type httpUpgrade struct {
	request     handshake.Request
	protocol    string
	extensions  string
	compression compress.Params
}

// ServeHTTP upgrades a plain HTTP/1.1 TCP connection and transfers it from
// net/http to the Server's UIO event loops. Serve must already be running. TLS
// and HTTP/2 are not supported.
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request == nil {
		writeHTTPError(writer, http.StatusBadRequest, httpErrorInvalidUpgrade)
		return
	}
	if !s.ready.Load() {
		writeHTTPError(writer, http.StatusInternalServerError, httpErrorServerNotServing)
		return
	}
	if request.TLS != nil || request.ProtoMajor != 1 {
		writeHTTPError(writer, http.StatusNotImplemented, httpErrorUnsupportedHTTP)
		return
	}

	config := s.config
	parsed, err := handshake.ValidateServerRequest(request, handshake.ServerOptions{
		CheckOrigin: config.checkOrigin,
	})
	if err != nil {
		writeHTTPError(writer, http.StatusBadRequest, httpErrorInvalidUpgrade)
		return
	}
	upgrade, err := prepareHTTPUpgrade(parsed, config)
	if err != nil {
		writeHTTPError(writer, http.StatusBadRequest, httpErrorInvalidUpgrade)
		return
	}
	if !s.ready.Load() {
		writeHTTPError(writer, http.StatusInternalServerError, httpErrorServerNotServing)
		return
	}

	conn, buffered, err := http.NewResponseController(writer).Hijack()
	if err != nil {
		writeHTTPError(writer, http.StatusInternalServerError, httpErrorCannotHijack)
		return
	}
	if buffered == nil || buffered.Reader.Buffered() != 0 || buffered.Writer.Buffered() != 0 {
		rejectHijacked(conn, buffered, httpErrorEarlyData)
		return
	}
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		rejectHijacked(conn, buffered, httpErrorUnsupportedHTTP)
		return
	}

	wsConn := s.newConnection(nil)
	wsConn.handshake.Store(&handshakeState{upgrade: upgrade})
	_, err = s.Events.Adopt(tcpConn, wsConn)
	if err != nil {
		return
	}
}

func prepareHTTPUpgrade(request handshake.Request, config *connConfig) (*httpUpgrade, error) {
	protocol := handshake.SelectSubprotocol(request.Subprotocols, config.subprotocols)
	params, extensions, err := extension.NegotiateServerWithPolicy(
		request.Extensions,
		config.compressionEnabled,
		!config.compressionContextTakeover,
	)
	if err != nil {
		return nil, err
	}
	params.Level = config.compressionLevel
	return &httpUpgrade{
		request:     request,
		protocol:    protocol,
		extensions:  extensions,
		compression: params,
	}, nil
}

func (s *Server) openHTTPConnection(conn *Conn, state *handshakeState) {
	raw := conn.raw
	upgrade := state.upgrade
	if upgrade.compression.Enabled {
		params := upgrade.compression
		conn.compression = &compressionState{
			encoder: compress.NewEncoderWithWindow(params.Level, params.ServerNoContextTakeover, params.ServerMaxWindowBits),
			decoder: compress.NewDecoderWithWindow(params.ClientNoContextTakeover, params.ClientMaxWindowBits),
		}
	}

	response := uio.AcquireBuffer(handshake.ServerResponseSize(upgrade.protocol, upgrade.extensions))
	wire := handshake.AppendServerResponse(response.AvailableBuffer()[:0], upgrade.request, upgrade.protocol, upgrade.extensions)
	response.CommitWrite(len(wire))
	if err := conn.writeTransportOwned(response); err != nil {
		_ = raw.CloseWith(err)
		return
	}
	if err := raw.Flush(); err != nil {
		_ = raw.CloseWith(err)
		return
	}
	conn.setSubprotocol(upgrade.protocol)
	if !conn.markOpened() {
		_ = raw.CloseWith(ErrClosed)
		return
	}
	conn.releaseHandshakeState(state)
	if conn.heartbeat != nil {
		conn.heartbeat.lastPong.Store(time.Now().UnixNano())
		conn.config.heartbeatConnections.Store(conn, conn)
	}
	if err := conn.dispatchOpen(); err != nil {
		_ = raw.CloseWith(err)
	}
}

func writeHTTPError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Connection", "close")
	http.Error(writer, message, status)
}

func rejectHijacked(conn net.Conn, buffered *bufio.ReadWriter, message string) {
	if conn == nil {
		return
	}
	if buffered != nil && buffered.Writer.Buffered() == 0 {
		body := message + "\n"
		_, _ = buffered.WriteString("HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Type: text/plain; charset=utf-8\r\nX-Content-Type-Options: nosniff\r\nContent-Length: ")
		_, _ = buffered.WriteString(strconv.Itoa(len(body)))
		_, _ = buffered.WriteString("\r\n\r\n")
		_, _ = buffered.WriteString(body)
		_ = buffered.Flush()
	}
	_ = conn.Close()
}
