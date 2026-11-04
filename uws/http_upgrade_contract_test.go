package uws

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urpc/uio/uws/internal/handshake"
)

func TestHTTPUpgradeRejectsInvalidRequestsBeforeHijack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request func() *http.Request
		want    int
	}{
		{"nil", func() *http.Request { return nil }, http.StatusBadRequest},
		{"tls", func() *http.Request { r := newHTTPUpgradeRequest(); r.TLS = new(tls.ConnectionState); return r }, http.StatusNotImplemented},
		{"http2", func() *http.Request { r := newHTTPUpgradeRequest(); r.ProtoMajor = 2; return r }, http.StatusNotImplemented},
		{"invalid-upgrade", func() *http.Request { r := newHTTPUpgradeRequest(); r.Header.Del("Upgrade"); return r }, http.StatusBadRequest},
		{"invalid-extension", func() *http.Request {
			r := newHTTPUpgradeRequest()
			r.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate; server_max_window_bits=7")
			return r
		}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Options{EnableCompression: true})
			s.config = testServerConfig(s)
			s.ready.Store(true)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, tc.request())
			if w.Code != tc.want || w.Header().Get("Connection") != "close" {
				t.Fatalf("status=%d headers=%v", w.Code, w.Header())
			}
		})
	}
}

func TestHTTPUpgradeRechecksServerAfterAuthorization(t *testing.T) {
	s := NewServer(Options{})
	s.Authorize = func(*http.Request) (any, error) { s.ready.Store(false); return nil, nil }
	s.config = testServerConfig(s)
	s.ready.Store(true)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, newHTTPUpgradeRequest())
	if w.Code != http.StatusInternalServerError {
		t.Fatal(w.Code)
	}
}

// Hijacking may be supplied by middleware over a non-TCP stream. An upgrade
// must reject and close that stream instead of passing it to socket adoption.
type contractHijacker struct {
	*httptest.ResponseRecorder
	conn   net.Conn
	buffer *bufio.ReadWriter
}

func (w contractHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.conn, w.buffer, nil }
func TestHTTPUpgradeRejectsNonTCPHijack(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	body := new(bytes.Buffer)
	w := contractHijacker{httptest.NewRecorder(), a, bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(nil)), bufio.NewWriter(body))}
	s := NewServer(Options{})
	s.config = testServerConfig(s)
	s.ready.Store(true)
	s.ServeHTTP(w, newHTTPUpgradeRequest())
	if _, err := b.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Contains(body.Bytes(), []byte("400 Bad Request")) {
		t.Fatal(body.String())
	}
	rejectHijacked(nil, nil, "ignored")
}

func TestAdoptedUpgradeWriteFailuresNeverPublishOpen(t *testing.T) {
	for _, tc := range []struct {
		name               string
		writeErr, flushErr error
		expired            bool
	}{
		{"write", io.ErrClosedPipe, nil, false}, {"flush", nil, io.ErrClosedPipe, false}, {"expired", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := newScriptedConn()
			raw.writeErr = tc.writeErr
			raw.flushErr = tc.flushErr
			raw.closeCause = make(chan error, 1)
			h := new(recordingHandler)
			c := &Conn{raw: raw, handler: h, config: testServerConfig(nil)}
			state := &handshakeState{expired: tc.expired, upgrade: &httpUpgrade{request: handshake.Request{Key: testKey}}}
			c.handshake.Store(state)
			new(Server).openHTTPConnection(c, state)
			if c.opened.Load() || len(h.events) != 0 || raw.closes != 1 {
				t.Fatalf("opened=%v events=%v closes=%d", c.opened.Load(), h.events, raw.closes)
			}
			c.stopHandshakeTimer()
		})
	}
}
