package uws

import (
	"bufio"
	"bytes"
	"io"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

const testKey = "dGhlIHNhbXBsZSBub25jZQ=="

func testServerConfig(server *Server) *connConfig {
	if server == nil {
		server = NewServer(Options{})
	}
	opts, err := server.Options.resolve()
	if err != nil {
		panic(err)
	}
	return newServerConnConfig(opts, server.CheckOrigin, server.Authorize, &server.connections, &server.stats)
}

func testDialerConfig(dialer *Dialer) *connConfig {
	if dialer == nil {
		dialer = NewDialer(Options{})
	}
	opts, err := dialer.Options.resolve()
	if err != nil {
		panic(err)
	}
	return newDialerConnConfig(opts, &dialer.connections, &dialer.stats)
}

func testIOTimeout() time.Duration {
	if testing.CoverMode() == "atomic" {
		return 15 * time.Second
	}
	return 5 * time.Second
}

type echoHandler struct {
	open    chan struct{}
	closed  chan struct{}
	message chan Message
	conn    chan *Conn
	once    sync.Once
}

type clientHandler struct {
	name    string
	conn    *Conn // set by OnOpen before open closes
	open    chan struct{}
	closed  chan struct{}
	message chan Message
}

type fragmentedMessageHandler struct {
	ready chan error
}

type compressedMessageHandler struct {
	ready chan error
}

type heartbeatHandler struct {
	closed chan CloseEvent
}

type reentrantCloseHandler struct {
	open    chan struct{}
	closed  chan struct{}
	closeMu sync.RWMutex
	closeFn func()
	once    sync.Once
}

type writeProbeConn struct {
	uio.Conn
	writes        int
	writevs       int
	flushes       int
	wakes         int
	wakeErr       error
	outbound      int // reported by OutboundBuffered
	closes        int
	closed        chan struct{}
	closeCause    chan error
	closeOnce     sync.Once
	deadline      time.Time
	readDeadline  time.Time
	writeDeadline time.Time
	deadlineErr   error
	noDelay       bool
	noDelayErr    error
}

type bufferedProbeConn struct {
	writeProbeConn
	inbound []byte
}

// OutboundBuffered reports the queued output tests simulate.
func (c *writeProbeConn) OutboundBuffered() int { return c.outbound }

func (c *bufferedProbeConn) YieldRead() error { return c.Wake() }

func (c *bufferedProbeConn) InboundBuffered() int { return len(c.inbound) }

func (c *bufferedProbeConn) Peek(dst []byte) []byte {
	n := copy(dst, c.inbound)
	return dst[:n]
}

func (c *bufferedProbeConn) PeekChunk() []byte { return c.inbound }

func (c *bufferedProbeConn) Discard(n int) (int, error) {
	if n < 0 || n > len(c.inbound) {
		n = len(c.inbound)
	}
	c.inbound = c.inbound[n:]
	return n, nil
}

type segmentedProbeConn struct {
	writeProbeConn
	segments [][]byte
	buffered int
}

func (c *segmentedProbeConn) YieldRead() error { return c.Wake() }

func newSegmentedProbeConn(segments ...[]byte) *segmentedProbeConn {
	c := &segmentedProbeConn{segments: segments}
	for _, segment := range segments {
		c.buffered += len(segment)
	}
	return c
}

func (c *segmentedProbeConn) InboundBuffered() int { return c.buffered }

func (c *segmentedProbeConn) Peek(dst []byte) []byte {
	written := 0
	for _, segment := range c.segments {
		written += copy(dst[written:], segment)
		if written == len(dst) {
			break
		}
	}
	return dst[:written]
}

func (c *segmentedProbeConn) PeekChunk() []byte {
	for len(c.segments) > 0 && len(c.segments[0]) == 0 {
		c.segments[0] = nil
		c.segments = c.segments[1:]
	}
	if len(c.segments) == 0 {
		return nil
	}
	return c.segments[0]
}

func (c *segmentedProbeConn) Discard(n int) (int, error) {
	if n < 0 || n > c.buffered {
		n = c.buffered
	}
	discarded := n
	c.buffered -= n
	for n > 0 && len(c.segments) > 0 {
		if n < len(c.segments[0]) {
			c.segments[0] = c.segments[0][n:]
			break
		}
		n -= len(c.segments[0])
		c.segments[0] = nil
		c.segments = c.segments[1:]
	}
	return discarded, nil
}

type assemblerStateHandler struct {
	assemblerVisible bool
	messages         []string
}

func (*assemblerStateHandler) OnOpen(*Conn) {}

func (h *assemblerStateHandler) OnMessage(conn *Conn, message Message) {
	h.assemblerVisible = conn.assembler != nil
	h.messages = append(h.messages, string(message.Payload))
}

func (*assemblerStateHandler) OnClose(*Conn, CloseEvent) {}

type recordingHandler struct {
	mu       sync.Mutex
	messages []string
	events   []string
}

func (h *recordingHandler) OnOpen(*Conn) {
	h.mu.Lock()
	h.events = append(h.events, "open")
	h.mu.Unlock()
}

func (h *recordingHandler) OnMessage(_ *Conn, message Message) {
	h.mu.Lock()
	h.messages = append(h.messages, string(message.Payload))
	h.events = append(h.events, string(message.Payload))
	h.mu.Unlock()
}

func (h *recordingHandler) OnClose(*Conn, CloseEvent) {
	h.mu.Lock()
	h.events = append(h.events, "close")
	h.mu.Unlock()
}

func (c *writeProbeConn) Write(payload []byte) (int, error) {
	c.writes++
	return len(payload), nil
}

func (c *writeProbeConn) Writev(buffers [][]byte) (int, error) {
	c.writes++
	c.writevs++
	total := 0
	for _, buffer := range buffers {
		total += len(buffer)
	}
	return total, nil
}

func (c *writeProbeConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	c.writes++
	n := buffer.Len()
	uio.ReleaseBuffer(buffer)
	return n, nil
}

// ReserveOutbound sends probes down the owned-write path, which they count.
func (c *writeProbeConn) ReserveOutbound(int) ([]byte, error) {
	return nil, uio.ErrReserveUnsupported
}

func (c *writeProbeConn) Flush() error {
	c.flushes++
	return nil
}

func (c *writeProbeConn) Wake() error {
	c.wakes++
	return c.wakeErr
}

func (c *writeProbeConn) SetDeadline(deadline time.Time) error {
	c.deadline = deadline
	return c.deadlineErr
}

func (c *writeProbeConn) SetReadDeadline(deadline time.Time) error {
	c.readDeadline = deadline
	return c.deadlineErr
}

func (c *writeProbeConn) SetWriteDeadline(deadline time.Time) error {
	c.writeDeadline = deadline
	return c.deadlineErr
}

func (c *writeProbeConn) SetNoDelay(noDelay bool) error {
	c.noDelay = noDelay
	return c.noDelayErr
}

func (c *writeProbeConn) CloseWith(err error) error {
	c.closes++
	if c.closeCause != nil {
		select {
		case c.closeCause <- err:
		default:
		}
	}
	if c.closed != nil {
		c.closeOnce.Do(func() { close(c.closed) })
	}
	return nil
}

func completeTestOutbound(conn *Conn) {
	pending := conn.writes.close.pendingBytes.Load()
	if pending > 0 {
		conn.releaseOutbound(int(pending))
	}
	_, _ = conn.tryCloseTransport()
}

func (*heartbeatHandler) OnOpen(*Conn)                       {}
func (*heartbeatHandler) OnMessage(*Conn, Message)           {}
func (h *heartbeatHandler) OnClose(_ *Conn, info CloseEvent) { h.closed <- info }

func (h *reentrantCloseHandler) OnOpen(*Conn) {
	if h.open != nil {
		close(h.open)
	}
}

func (*reentrantCloseHandler) OnMessage(*Conn, Message) {}

func (h *reentrantCloseHandler) OnClose(*Conn, CloseEvent) {
	h.closeMu.RLock()
	closeFn := h.closeFn
	h.closeMu.RUnlock()
	if closeFn != nil {
		closeFn()
	}
	h.once.Do(func() {
		if h.closed != nil {
			close(h.closed)
		}
	})
}

func (h *reentrantCloseHandler) setCloseFunc(closeFn func()) {
	h.closeMu.Lock()
	h.closeFn = closeFn
	h.closeMu.Unlock()
}

func (h *fragmentedMessageHandler) OnOpen(conn *Conn) {
	h.ready <- conn.SendBinary([]byte("abcdef"))
}

func (*fragmentedMessageHandler) OnMessage(*Conn, Message)  {}
func (*fragmentedMessageHandler) OnClose(*Conn, CloseEvent) {}

func (h *compressedMessageHandler) OnOpen(conn *Conn) {
	h.ready <- conn.SendBinary(bytes.Repeat([]byte("compressed-message-"), 32))
}

func (*compressedMessageHandler) OnMessage(*Conn, Message)  {}
func (*compressedMessageHandler) OnClose(*Conn, CloseEvent) {}

func (h *clientHandler) OnOpen(c *Conn) { h.conn = c; close(h.open) }

func (h *clientHandler) OnMessage(_ *Conn, message Message) { h.message <- message.Clone() }

func (h *clientHandler) OnClose(*Conn, CloseEvent) { close(h.closed) }

func (h *echoHandler) OnOpen(conn *Conn) {
	if h.conn != nil {
		h.conn <- conn
	}
	close(h.open)
}

func (h *echoHandler) OnMessage(conn *Conn, message Message) {
	h.message <- message.Clone()
	_ = conn.SendText([]byte("world"))
}

func (h *echoHandler) OnClose(*Conn, CloseEvent) { h.once.Do(func() { close(h.closed) }) }

func readServerFrame(reader *bufio.Reader) (first byte, payload []byte, fin bool, err error) {
	var header [2]byte
	if _, err = io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, false, err
	}
	first, lengthCode := header[0], header[1]&0x7f
	fin = first&0x80 != 0
	var length uint64
	switch lengthCode {
	case 126:
		var extended [2]byte
		if _, err = io.ReadFull(reader, extended[:]); err != nil {
			return 0, nil, false, err
		}
		length = uint64(extended[0])<<8 | uint64(extended[1])
	case 127:
		var extended [8]byte
		if _, err = io.ReadFull(reader, extended[:]); err != nil {
			return 0, nil, false, err
		}
		for _, b := range extended {
			length = length<<8 | uint64(b)
		}
	default:
		length = uint64(lengthCode)
	}
	if length > uint64(^uint(0)>>1) {
		return 0, nil, false, io.ErrShortBuffer
	}
	payload = make([]byte, int(length))
	_, err = io.ReadFull(reader, payload)
	return first, payload, fin, err
}

// startTestServer binds and starts the server built by newServer on a fresh
// loopback port and returns it with its address and Serve's result. The
// listeners are bound before it returns: ready is set at the start of OnStart,
// after the bind. The caller owns shutdown and must drain serveDone.
//
// The port is picked below the ephemeral ranges (32768 up on Linux, 49152 up
// on macOS and Windows). A port probed from those ranges and released is
// handed to the next outgoing connection of any process on the host, and
// processes running tests side by side dial thousands. Below them only an
// explicit bind can take a port, and a taken one is retried.
func startTestServer(t *testing.T, newServer func() *Server) (*Server, string, <-chan error) {
	t.Helper()
	var lastErr error
attempt:
	for attempt := 0; attempt < 20; attempt++ {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(20000+rand.IntN(12000)))
		server := newServer()
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(addr) }()
		deadline := time.Now().Add(testIOTimeout())
		for {
			select {
			case lastErr = <-serveDone:
				// Serve gave up before binding: the port is in use.
				continue attempt
			default:
			}
			if server.ready.Load() {
				return server, addr, serveDone
			}
			if time.Now().After(deadline) {
				t.Fatalf("server did not bind %s: %v", addr, lastErr)
			}
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatalf("could not bind a loopback port: %v", lastErr)
	return nil, "", nil
}

// stopTestServer closes server and waits for Serve to return.
func stopTestServer(t *testing.T, server *Server, serveDone <-chan error) {
	t.Helper()
	_ = server.Close(nil)
	select {
	case <-serveDone:
	case <-time.After(testIOTimeout()):
		t.Error("server did not stop")
	}
}
