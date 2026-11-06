package uws

import (
	"bufio"
	"github.com/urpc/uio"
	"io"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
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
	return newServerConnConfig(opts, server.CheckOrigin, server.Authorize)
}

func testDialerConfig(dialer *Dialer) *connConfig {
	if dialer == nil {
		dialer = NewDialer(Options{})
	}
	opts, err := dialer.Options.resolve()
	if err != nil {
		panic(err)
	}
	return newDialerConnConfig(opts)
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
	conn    *Conn
	open    chan struct{}
	closed  chan struct{}
	message chan Message
} // set by OnOpen before open closes

type writeProbeConn struct {
	probeReadAccess
	uio.Conn
	uio.SocketConn
	probeReadControl
	writes        int
	writevs       int
	flushes       int
	wakes         int
	wakeErr       error
	outbound      int
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
	wakeMu        sync.Mutex
	wakesAt       []time. // reported by OutboundBuffered
		// wakeMu guards wakesAt, the deadlines the heartbeat armed. A test reads
		// it from its own goroutine; the fake itself records no other way.
		Time
	wakeArmErr error
} // SetWakeDeadline's result, recorded or not

type bufferedProbeConn struct {
	writeProbeConn
	inbound []byte
}

func (c *writeProbeConn) OutboundBuffered() int {
	return c.outbound
}

func (c *bufferedProbeConn) InboundBuffered() int {
	return len(c.inbound)
}

func (c *bufferedProbeConn) Peek(dst []byte) []byte {
	n := copy(dst, c.inbound)
	return dst[:n]
}

func (c *bufferedProbeConn) PeekChunk() []byte {
	return c.inbound
}

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

func newSegmentedProbeConn(segments ...[]byte) *segmentedProbeConn {
	c := &segmentedProbeConn{segments: segments}
	for _, segment := range segments {
		c.buffered += len(segment)
	}
	return c
}

func (c *segmentedProbeConn) InboundBuffered() int {
	return c.buffered
}

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

func (c *writeProbeConn) Flush() error {
	c.flushes++
	return nil
}

func (c *writeProbeConn) SetWakeDeadline(when time.Time) error {
	c.wakeMu.Lock()
	defer c.wakeMu.Unlock()
	c.wakesAt = append(c.wakesAt, when)
	return c.wakeArmErr
}

func (c *writeProbeConn) SetWakeInterval(time.Duration) error {
	return nil
}

func (c *writeProbeConn) wakeArms() []time.Time {
	c.wakeMu.Lock()
	defer c.wakeMu.Unlock()
	return append([]time.Time(nil), c.wakesAt...)
}

func (c *writeProbeConn) lastWakeArm() (time.Time, bool) {
	c.wakeMu.Lock()
	defer c.wakeMu.Unlock()
	if len(c.wakesAt) == 0 {
		return time.Time{}, false
	}
	return c.wakesAt[len(c.wakesAt)-1], true
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

func (c *writeProbeConn) Close() error {
	return c.Abort(io.EOF)
}

func (c *writeProbeConn) Abort(err error) error {
	c.closes++
	if c.closeCause != nil {
		select {
		case c.closeCause <- err:
		default:
		}
	}
	if c.closed != nil {
		c.closeOnce.Do(func() {
			close(c.closed)
		})
	}
	return nil
}

func completeTestOutbound(conn *Conn) {
	drained := 0
	switch raw := conn.raw.(type) {
	case *scriptedConn:
		drained, raw.outbound = raw.outbound, 0
	case *heartbeatWriteProbe:
		drained, raw.outbound = raw.outbound, 0
	case *writeProbeConn:
		drained, raw.outbound = raw.outbound, 0
	}
	conn.onTransportProgress(drained)
	_, _ = conn.tryCloseTransport()
}

func (h *clientHandler) OnOpen(c *Conn) {
	h.conn = c
	close(h.open)
}

func (h *clientHandler) OnMessage(_ *Conn, message Message) {
	h.message <- message.Clone()
}

func (h *clientHandler) OnClose(*Conn, CloseEvent) {
	close(h.closed)
}

func (h *echoHandler) OnOpen(conn *Conn) {
	if h.conn != nil {
		h.conn <- conn
	}
	close(h.open)
}

func (h *echoHandler) OnMessage(conn *Conn, message Message) {
	h.message <- message.Clone()
	_ = conn.SendText("world")
}

func (h *echoHandler) OnClose(*Conn, CloseEvent) {
	h.once.Do(func() {
		close(h.closed)
	})
}

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

func startTestServer(t *testing.T, newServer func() *Server) (*Server, string, <-chan error) {
	t.Helper()
	var lastErr error
attempt:
	for attempt := 0; attempt < 20; attempt++ {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(20000+rand.IntN(12000)))
		server := newServer()
		serveDone := make(chan error, 1)
		go func() {
			serveDone <- server.Serve(addr)
		}()
		deadline := time.Now().Add(testIOTimeout())
		for {
			select {
			case lastErr = <-serveDone:
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

func stopTestServer(t *testing.T, server *Server, serveDone <-chan error) {
	t.Helper()
	_ = server.Close(nil)
	select {
	case <-serveDone:
	case <-time.After(testIOTimeout()):
		t.Error("server did not stop")
	}
}
