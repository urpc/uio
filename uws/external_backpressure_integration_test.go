package uws

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/uws/internal/frame"
)

// These tests use real transports: a Resume must wake already buffered input
// even when the peer makes no further write.
type externalReadControlHandler struct {
	open    func(*Conn)
	message func(*Conn, Message)
	closed  func(*Conn, CloseEvent)
	inbound func(*Conn, int)
}

func (h *externalReadControlHandler) OnOpen(c *Conn) {
	if h.open != nil {
		h.open(c)
	}
}

func (h *externalReadControlHandler) OnMessage(c *Conn, m Message) {
	if h.message != nil {
		h.message(c, m)
	}
}

func (h *externalReadControlHandler) OnClose(c *Conn, event CloseEvent) {
	if h.closed != nil {
		h.closed(c, event)
	}
}

func (h *externalReadControlHandler) OnInbound(c *Conn, n int) {
	if h.inbound != nil {
		h.inbound(c, n)
	}
}

func (*externalReadControlHandler) OnOutbound(*Conn, int) {}

func waitExternalReadControl[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(testIOTimeout()):
		t.Fatalf("timed out waiting for %s", what)
		return *new(T)
	}
}

func dialExternalReadControl(t *testing.T, addr string, tlsConfig *tls.Config, tail []byte) (net.Conn, *bufio.Reader) {
	t.Helper()
	var peer net.Conn
	var err error
	dialer := &net.Dialer{Timeout: testIOTimeout()}
	if tlsConfig == nil {
		peer, err = dialer.Dial("tcp", addr)
	} else {
		peer, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	if err := peer.SetDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	request := "GET / HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
	wire := append([]byte(request), tail...)
	if n, err := peer.Write(wire); err != nil || n != len(wire) {
		t.Fatalf("write upgrade and trailing frames: %d/%d bytes, %v", n, len(wire), err)
	}
	reader := bufio.NewReader(peer)
	if line, err := reader.ReadString('\n'); err != nil || len(line) < 12 || line[:12] != "HTTP/1.1 101" {
		t.Fatalf("handshake response = %q, %v", line, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	return peer, reader
}

func externalReadControlCases(t *testing.T, test func(*testing.T, *tls.Config, *tls.Config)) {
	t.Helper()
	t.Run("ws", func(t *testing.T) { test(t, nil, nil) })
	t.Run("wss", func(t *testing.T) {
		serverTLS, clientTLS := wssConfigs(t)
		test(t, serverTLS, clientTLS)
	})
}

func TestExternalPauseResumeSocketBufferedFrames(t *testing.T) {
	externalReadControlCases(t, func(t *testing.T, serverTLS, clientTLS *tls.Config) {
		for _, high := range []int{0, -1} {
			t.Run(fmt.Sprintf("automatic_high=%d", high), func(t *testing.T) {
				opened := make(chan *Conn, 1)
				paused := make(chan error, 1)
				messages := make(chan string, 3)
				calls := 0
				handler := &externalReadControlHandler{
					open: func(c *Conn) { opened <- c },
					message: func(c *Conn, m Message) {
						calls++
						if calls == 1 {
							paused <- c.PauseReads()
						}
						messages <- string(m.Payload)
						if err := c.Send(m.Type, m.Payload); err != nil {
							t.Error(err)
						}
					},
				}
				server, addr, done := startTestServer(t, func() *Server {
					return NewServer(Options{Handler: handler, TLSConfig: serverTLS, Pollers: 1,
						OutboundHighWatermark: high})
				})
				t.Cleanup(func() { stopTestServer(t, server, done) })
				peer, reader := dialExternalReadControl(t, addr, clientTLS, nil)
				conn := waitExternalReadControl(t, opened, "server connection")
				wire := maskedFrames("one", "two", "three")
				if n, err := peer.Write(wire); err != nil || n != len(wire) {
					t.Fatalf("write frames: %d/%d, %v", n, len(wire), err)
				}
				if err := waitExternalReadControl(t, paused, "PauseReads result"); err != nil {
					t.Fatal(err)
				}
				if payload, err := readEchoFrame(reader); err != nil || string(payload) != "one" {
					t.Fatalf("first echo = %q, %v", payload, err)
				}
				if message := waitExternalReadControl(t, messages, "first message"); message != "one" {
					t.Fatalf("first message = %q", message)
				}
				select {
				case message := <-messages:
					t.Fatalf("paused connection delivered %q", message)
				default:
				}
				if err := conn.ResumeReads(); err != nil {
					t.Fatal(err)
				}
				// No more peer writes: the wake must deliver what is buffered.
				for _, want := range []string{"two", "three"} {
					if payload, err := readEchoFrame(reader); err != nil || string(payload) != want {
						t.Fatalf("resumed echo = %q, %v; want %q", payload, err, want)
					}
					if message := waitExternalReadControl(t, messages, "resumed message"); message != want {
						t.Fatalf("resumed message = %q, want %q", message, want)
					}
				}
			})
		}
	})
}

func TestExternalPauseOnOpenPreservesSocketHandshakeTail(t *testing.T) {
	externalReadControlCases(t, func(t *testing.T, serverTLS, clientTLS *tls.Config) {
		opened := make(chan *Conn, 1)
		paused := make(chan error, 1)
		messages := make(chan string, 2)
		handler := &externalReadControlHandler{
			open: func(c *Conn) {
				paused <- c.PauseReads()
				opened <- c
				if err := c.SendText("paused"); err != nil {
					t.Error(err)
				}
			},
			message: func(_ *Conn, m Message) { messages <- string(m.Payload) },
		}
		server, addr, done := startTestServer(t, func() *Server {
			return NewServer(Options{Handler: handler, TLSConfig: serverTLS, Pollers: 1})
		})
		t.Cleanup(func() { stopTestServer(t, server, done) })
		_, reader := dialExternalReadControl(t, addr, clientTLS, maskedFrames("one", "two"))
		conn := waitExternalReadControl(t, opened, "paused OnOpen connection")
		if err := waitExternalReadControl(t, paused, "OnOpen PauseReads result"); err != nil {
			t.Fatal(err)
		}
		if payload, err := readEchoFrame(reader); err != nil || string(payload) != "paused" {
			t.Fatalf("OnOpen marker = %q, %v", payload, err)
		}
		select {
		case message := <-messages:
			t.Fatalf("OnOpen pause delivered handshake tail %q", message)
		default:
		}
		if err := conn.ResumeReads(); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"one", "two"} {
			if got := waitExternalReadControl(t, messages, "handshake-tail message"); got != want {
				t.Fatalf("handshake-tail message = %q, want %q", got, want)
			}
		}
	})
}

func TestExternalPausedSocketStillClosesGracefully(t *testing.T) {
	externalReadControlCases(t, func(t *testing.T, serverTLS, clientTLS *tls.Config) {
		opened := make(chan *Conn, 1)
		paused := make(chan error, 1)
		closed := make(chan CloseEvent, 1)
		var calls atomic.Int32
		handler := &externalReadControlHandler{
			open: func(c *Conn) { opened <- c },
			message: func(c *Conn, m Message) {
				if calls.Add(1) == 1 {
					paused <- c.PauseReads()
					if err := c.SendBinary(m.Payload); err != nil {
						t.Error(err)
					}
				}
			},
			closed: func(_ *Conn, event CloseEvent) { closed <- event },
		}
		server, addr, done := startTestServer(t, func() *Server {
			return NewServer(Options{Handler: handler, TLSConfig: serverTLS, Pollers: 1})
		})
		t.Cleanup(func() { stopTestServer(t, server, done) })
		peer, reader := dialExternalReadControl(t, addr, clientTLS, nil)
		conn := waitExternalReadControl(t, opened, "connection to close")
		if _, err := peer.Write(maskedFrames("one", "two")); err != nil {
			t.Fatal(err)
		}
		if err := waitExternalReadControl(t, paused, "PauseReads before Close"); err != nil {
			t.Fatal(err)
		}
		if payload, err := readEchoFrame(reader); err != nil || string(payload) != "one" {
			t.Fatalf("first echo = %q, %v", payload, err)
		}
		if err := conn.Close(1000, "done"); err != nil {
			t.Fatal(err)
		}
		opcode, payload, err := readFrameWithOpcode(reader)
		if err != nil || opcode != frame.Close {
			t.Fatalf("close frame = %v %q, %v", opcode, payload, err)
		}
		wire := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Close, Masked: true, Payload: payload}, [4]byte{1, 2, 3, 4})
		if _, err := peer.Write(wire); err != nil {
			t.Fatal(err)
		}
		if event := waitExternalReadControl(t, closed, "graceful close while paused"); event.Code != 1000 {
			t.Fatalf("close event = %+v", event)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("closing connection delivered %d messages, want only the first", got)
		}
	})
}

func TestConcurrentExternalPauseResumeSocketKeepsMessages(t *testing.T) {
	opened := make(chan *Conn, 1)
	paused := make(chan error, 1)
	const count = 200
	messages := make(chan string, count)
	calls := 0
	handler := &externalReadControlHandler{
		open: func(c *Conn) { opened <- c },
		message: func(c *Conn, m Message) {
			calls++
			if calls == 1 {
				paused <- c.PauseReads()
			}
			messages <- string(m.Payload)
		},
	}
	server, addr, done := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: handler, Pollers: 1})
	})
	t.Cleanup(func() { stopTestServer(t, server, done) })
	peer, _ := dialExternalReadControl(t, addr, nil, nil)
	conn := waitExternalReadControl(t, opened, "connection to race pause/resume")
	if _, err := peer.Write(maskedFrames("0")); err != nil {
		t.Fatal(err)
	}
	if err := waitExternalReadControl(t, paused, "first PauseReads"); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	failures := make(chan error, 2)
	for worker := range 2 {
		workers.Add(1)
		go func(reverse bool) {
			defer workers.Done()
			for range 500 {
				var err error
				if reverse {
					err = conn.ResumeReads()
				} else {
					err = conn.PauseReads()
				}
				if err != nil {
					failures <- err
					return
				}
			}
		}(worker != 0)
	}
	parts := make([]string, count-1)
	for i := range parts {
		parts[i] = fmt.Sprint(i + 1)
	}
	wire := maskedFrames(parts...)
	if n, err := peer.Write(wire); err != nil || n != len(wire) {
		t.Fatalf("write concurrent frames: %d/%d, %v", n, len(wire), err)
	}
	workers.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
	if err := conn.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	for i := range count {
		if message := waitExternalReadControl(t, messages, "message after concurrent pause/resume"); message != fmt.Sprint(i) {
			t.Fatalf("message %d = %q", i, message)
		}
	}
}

// Disabling automatic watermarks must not disable the transport's receive
// bound: a manual pause leaves frames unread and eventually stops the socket.
func TestExternalPauseBoundsSocketInputWithAutomaticPauseDisabled(t *testing.T) {
	const count = 128
	opened := make(chan *Conn, 1)
	paused := make(chan error, 1)
	bounded := make(chan int, 1)
	finished := make(chan struct{})
	var boundOnce sync.Once
	var seen atomic.Int32
	var pausedInput atomic.Bool
	handler := &externalReadControlHandler{
		open: func(c *Conn) { opened <- c },
		message: func(c *Conn, m Message) {
			n := seen.Add(1)
			if n == 1 {
				err := c.PauseReads()
				pausedInput.Store(err == nil)
				paused <- err
				if err := c.SendText("paused"); err != nil {
					t.Error(err)
				}
			}
			if n == count+1 {
				close(finished)
			}
		},
		inbound: func(c *Conn, _ int) {
			if pausedInput.Load() {
				if n := c.raw.InboundBuffered(); n >= DefaultOutboundHighWatermark {
					boundOnce.Do(func() { bounded <- n })
				}
			}
		},
	}
	server, addr, done := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: handler, Pollers: 1, ReadBufferSize: 4 << 10, OutboundHighWatermark: -1})
	})
	t.Cleanup(func() { stopTestServer(t, server, done) })
	if got := server.events.MaxInboundBuffered; got != DefaultOutboundHighWatermark {
		t.Fatalf("transport receive bound = %d, want %d", got, DefaultOutboundHighWatermark)
	}
	peer, reader := dialExternalReadControl(t, addr, nil, nil)
	conn := waitExternalReadControl(t, opened, "connection with automatic pause disabled")
	if _, err := peer.Write(maskedFrames("start")); err != nil {
		t.Fatal(err)
	}
	if err := waitExternalReadControl(t, paused, "manual pause with automatic pause disabled"); err != nil {
		t.Fatal(err)
	}
	if payload, err := readEchoFrame(reader); err != nil || string(payload) != "paused" {
		t.Fatalf("pause marker = %q, %v", payload, err)
	}
	parts := make([]string, count)
	for i := range parts {
		parts[i] = string(make([]byte, 4<<10))
	}
	wire := maskedFrames(parts...)
	writeDone := make(chan error, 1)
	go func() {
		n, err := peer.Write(wire)
		if err == nil && n != len(wire) {
			err = io.ErrShortWrite
		}
		writeDone <- err
	}()
	buffered := waitExternalReadControl(t, bounded, "receive hold at the configured bound")
	if buffered > DefaultOutboundHighWatermark+(4<<10) {
		t.Fatalf("paused input = %d, exceeds bound plus one read", buffered)
	}
	if got := seen.Load(); got != 1 {
		t.Fatalf("manual pause delivered %d messages while buffering", got)
	}
	pausedInput.Store(false)
	if err := conn.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if err := waitExternalReadControl(t, writeDone, "bulk peer write after resuming"); err != nil {
		t.Fatal(err)
	}
	waitExternalReadControl(t, finished, "all buffered messages after resuming")
}
