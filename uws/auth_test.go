package uws

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// authRecorder is a server Handler that records the Userdata each connection
// opened with, and counts the callbacks a refused connection must not get.
type authRecorder struct {
	mu        sync.Mutex
	userdata  []any
	callbacks int
	opened    chan struct{}
}

func newAuthRecorder() *authRecorder { return &authRecorder{opened: make(chan struct{}, 16)} }

func (h *authRecorder) OnOpen(c *Conn) {
	h.mu.Lock()
	h.userdata = append(h.userdata, c.Userdata())
	h.callbacks++
	h.mu.Unlock()
	h.opened <- struct{}{}
}

func (h *authRecorder) OnMessage(c *Conn, m Message) { _ = c.SendBinary(m.Payload) }

func (h *authRecorder) OnClose(*Conn, CloseEvent) {
	h.mu.Lock()
	h.callbacks++
	h.mu.Unlock()
}

func (h *authRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.callbacks
}

// tokenAuthorize accepts "Bearer <user>" and refuses everything else with a
// 401 that names the scheme.
func tokenAuthorize(r *http.Request) (any, error) {
	user, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || user == "" {
		return nil, &RejectError{
			Status: http.StatusUnauthorized,
			Header: http.Header{"Www-Authenticate": {"Bearer"}},
			Reason: "missing or bad token",
		}
	}
	return user, nil
}

func startAuthServer(t *testing.T, handler Handler, authorize func(*http.Request) (any, error)) string {
	t.Helper()
	server, addr, serveDone := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: handler})
		s.Authorize = authorize
		return s
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })
	return addr
}

func dialAuth(t *testing.T, dialer *Dialer, addr string) (*Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	return dialer.DialClient(ctx, "ws://"+addr+"/")
}

// The Dialer's Header reaches Authorize, whose userdata becomes the server
// connection's Userdata by OnOpen.
func TestAuthorizeAcceptsDialerHeader(t *testing.T) {
	recorder := newAuthRecorder()
	addr := startAuthServer(t, recorder, tokenAuthorize)
	dialer := &Dialer{Header: http.Header{"Authorization": {"Bearer alice"}}}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	client, err := dialAuth(t, dialer, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(1000, "") }()
	select {
	case <-recorder.opened:
	case <-time.After(testIOTimeout()):
		t.Fatal("the server connection did not open")
	}
	recorder.mu.Lock()
	got := recorder.userdata
	recorder.mu.Unlock()
	if len(got) != 1 || got[0] != "alice" {
		t.Fatalf("server Userdata = %v, want [alice]", got)
	}
}

// A RejectError reaches the client whole — status, header and reason — and
// the refused connection never reaches the server's Handler.
func TestAuthorizeRejectionReachesClient(t *testing.T) {
	for _, backendPath := range []string{"native", "net/http"} {
		t.Run(backendPath, func(t *testing.T) {
			recorder := newAuthRecorder()
			var addr string
			if backendPath == "native" {
				addr = startAuthServer(t, recorder, tokenAuthorize)
			} else {
				server := NewServer(Options{Handler: recorder})
				server.Authorize = tokenAuthorize
				serveWithoutListener(t, server)
				httpServer := httptest.NewServer(server)
				t.Cleanup(httpServer.Close)
				addr = strings.TrimPrefix(httpServer.URL, "http://")
			}
			dialer := &Dialer{Header: http.Header{"Authorization": {"Basic nope"}}}
			t.Cleanup(func() { _ = dialer.Close(nil) })
			_, err := dialAuth(t, dialer, addr)
			var refused *HandshakeError
			if !errors.As(err, &refused) {
				t.Fatalf("DialClient error = %v, want a HandshakeError", err)
			}
			if refused.StatusCode != http.StatusUnauthorized || refused.Reason != "missing or bad token" ||
				refused.Header.Get("Www-Authenticate") != "Bearer" {
				t.Fatalf("refusal = %d %q %v", refused.StatusCode, refused.Reason, refused.Header)
			}
			time.Sleep(20 * time.Millisecond)
			if n := recorder.count(); n != 0 {
				t.Fatalf("a refused connection made %d server callbacks", n)
			}
		})
	}
}

// Any other Authorize error refuses with 403.
func TestAuthorizePlainErrorRefusesWith403(t *testing.T) {
	addr := startAuthServer(t, newAuthRecorder(), func(*http.Request) (any, error) {
		return nil, errors.New("no")
	})
	dialer := &Dialer{}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	_, err := dialAuth(t, dialer, addr)
	var refused *HandshakeError
	if !errors.As(err, &refused) || refused.StatusCode != http.StatusForbidden {
		t.Fatalf("DialClient error = %v, want a 403 HandshakeError", err)
	}
}

// A nil *RejectError is still a non-nil error: it refuses with 403 on both
// server paths, bare or wrapped, and the server keeps serving.
func TestAuthorizeNilRejectErrorRefusesWith403(t *testing.T) {
	authorizers := map[string]func(*http.Request) (any, error){
		"bare": func(*http.Request) (any, error) {
			var reject *RejectError
			return nil, reject
		},
		"wrapped": func(*http.Request) (any, error) {
			var reject *RejectError
			return nil, fmt.Errorf("denied: %w", reject)
		},
	}
	for name, authorize := range authorizers {
		for _, backendPath := range []string{"native", "net/http"} {
			t.Run(name+"/"+backendPath, func(t *testing.T) {
				recorder := newAuthRecorder()
				var addr string
				if backendPath == "native" {
					addr = startAuthServer(t, recorder, authorize)
				} else {
					server := NewServer(Options{Handler: recorder})
					server.Authorize = authorize
					serveWithoutListener(t, server)
					httpServer := httptest.NewServer(server)
					t.Cleanup(httpServer.Close)
					addr = strings.TrimPrefix(httpServer.URL, "http://")
				}
				dialer := &Dialer{}
				t.Cleanup(func() { _ = dialer.Close(nil) })
				for range 2 {
					_, err := dialAuth(t, dialer, addr)
					var refused *HandshakeError
					if !errors.As(err, &refused) || refused.StatusCode != http.StatusForbidden {
						t.Fatalf("DialClient error = %v, want a 403 HandshakeError", err)
					}
				}
				if n := recorder.count(); n != 0 {
					t.Fatalf("a refused connection made %d server callbacks", n)
				}
			})
		}
	}
}

// RequestHeader gives each dial its own headers, over Header's of the same
// name whatever the case.
func TestRequestHeaderPerDial(t *testing.T) {
	recorder := newAuthRecorder()
	addr := startAuthServer(t, recorder, tokenAuthorize)
	dialer := NewDialer(Options{Handler: clientUserdataHandler{}})
	dialer.Header = http.Header{"Authorization": {"Bearer static"}}
	dialer.RequestHeader = func(userdata any) http.Header {
		return http.Header{"authorization": {"Bearer " + userdata.(*clientHandler).name}}
	}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	for _, name := range []string{"bob", "carol"} {
		handler := &clientHandler{name: name, open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
		if err := dialer.Dial(context.Background(), "ws://"+addr+"/", handler); err != nil {
			t.Fatal(err)
		}
		select {
		case <-handler.open:
		case <-handler.closed:
			t.Fatalf("dial %s was refused", name)
		case <-time.After(testIOTimeout()):
			t.Fatalf("dial %s did not open", name)
		}
		select {
		case <-recorder.opened:
		case <-time.After(testIOTimeout()):
			t.Fatalf("the server connection of %s did not open", name)
		}
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.userdata) != 2 || recorder.userdata[0] != "bob" || recorder.userdata[1] != "carol" {
		t.Fatalf("server Userdata = %v, want [bob carol]", recorder.userdata)
	}
}

// Headers the handshake sets itself are refused, in Header at the first Dial
// and in RequestHeader at the dial that returns them.
func TestDialerRefusesReservedHeaders(t *testing.T) {
	static := &Dialer{Header: http.Header{"Sec-WebSocket-Key": {"x"}}}
	t.Cleanup(func() { _ = static.Close(nil) })
	if err := static.Dial(context.Background(), "ws://127.0.0.1:1/", nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Dial with a reserved Header = %v, want %v", err, ErrInvalidOptions)
	}
	perDial := &Dialer{RequestHeader: func(any) http.Header { return http.Header{"Host": {"evil"}} }}
	t.Cleanup(func() { _ = perDial.Close(nil) })
	if err := perDial.Dial(context.Background(), "ws://127.0.0.1:1/", nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Dial with a reserved RequestHeader = %v, want %v", err, ErrInvalidOptions)
	}
	broken := &Dialer{Header: http.Header{"X-Token": {"a\r\nInjected: 1"}}}
	t.Cleanup(func() { _ = broken.Close(nil) })
	if err := broken.Dial(context.Background(), "ws://127.0.0.1:1/", nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Dial with a header value carrying CRLF = %v, want %v", err, ErrInvalidOptions)
	}
}

// A request that is not a valid upgrade gets its 400 before the connection
// closes: the answer is counted like any frame, so the graceful close waits
// for it on both backends.
func TestInvalidUpgradeAnswerArrivesBeforeClose(t *testing.T) {
	addr := startAuthServer(t, newAuthRecorder(), nil)
	for range 50 {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n\r\n")
		_ = conn.SetReadDeadline(time.Now().Add(testIOTimeout()))
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("no answer to an invalid upgrade: %v", err)
		}
		_ = response.Body.Close()
		_ = conn.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid upgrade answered %d, want 400", response.StatusCode)
		}
	}
}

// The request Authorize sees carries the peer's address on the native path
// too, as behind net/http.
func TestAuthorizeSeesRemoteAddr(t *testing.T) {
	remote := make(chan string, 4)
	addr := startAuthServer(t, newAuthRecorder(), func(r *http.Request) (any, error) {
		remote <- r.RemoteAddr
		return nil, nil
	})
	dialer := &Dialer{}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	client, err := dialAuth(t, dialer, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(1000, "") }()
	local := client.LocalAddr().String()
	select {
	case got := <-remote:
		if got != local {
			t.Fatalf("Authorize RemoteAddr = %q, want the peer's %q", got, local)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("Authorize did not run")
	}
}
