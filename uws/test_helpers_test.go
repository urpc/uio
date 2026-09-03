package uws

import (
	"bufio"
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

const testKey = "dGhlIHNhbXBsZSBub25jZQ=="

func testServerConfig(server *Server) *connConfig {
	if server == nil {
		server = NewServer(nil)
	}
	return newServerConnConfig(server)
}

func testDialerConfig(dialer *Dialer) *connConfig {
	if dialer == nil {
		dialer = NewDialer()
	}
	return newDialerConnConfig(dialer)
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
	open    chan struct{}
	closed  chan struct{}
	message chan Message
}

type streamHandler struct {
	ready chan error
}

type compressedStreamHandler struct {
	ready chan error
}

type heartbeatHandler struct {
	closed chan CloseEvent
}

type reentrantCloseHandler struct {
	open    chan struct{}
	closed  chan struct{}
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
	closes        int
	closed        chan struct{}
	closeCause    chan error
	closeOnce     sync.Once
	deadline      time.Time
	readDeadline  time.Time
	writeDeadline time.Time
	deadlineErr   error
}

type bufferedProbeConn struct {
	writeProbeConn
	inbound []byte
	paused  bool
}

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

func (c *bufferedProbeConn) IsReadPaused() bool { return c.paused }

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

type pauseAfterMessageHandler struct{ messages int }

func (h *pauseAfterMessageHandler) OnOpen(*Conn) {}

func (h *pauseAfterMessageHandler) OnMessage(conn *Conn, _ Message) {
	h.messages++
	conn.raw.(*bufferedProbeConn).paused = true
}

func (*pauseAfterMessageHandler) OnClose(*Conn, CloseEvent) {}

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

type queuedExecutor struct {
	mu    sync.Mutex
	tasks []func()
}

func (e *queuedExecutor) Submit(task func()) bool {
	e.mu.Lock()
	e.tasks = append(e.tasks, task)
	e.mu.Unlock()
	return true
}

func (e *queuedExecutor) runNext() bool {
	e.mu.Lock()
	if len(e.tasks) == 0 {
		e.mu.Unlock()
		return false
	}
	task := e.tasks[0]
	e.tasks[0] = nil
	e.tasks = e.tasks[1:]
	e.mu.Unlock()
	task()
	return true
}

func (e *queuedExecutor) pending() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.tasks)
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

type rejectingExecutor struct{}

func (rejectingExecutor) Submit(func()) bool { return false }

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
	pending := conn.pendingBytes.Load()
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
	if h.closeFn != nil {
		h.closeFn()
	}
	h.once.Do(func() {
		if h.closed != nil {
			close(h.closed)
		}
	})
}

func (h *streamHandler) OnOpen(conn *Conn) {
	writer, err := conn.BeginMessage(BinaryMessage)
	if err == nil {
		_, err = writer.Write([]byte("abc"))
	}
	if err == nil {
		_, err = writer.Write([]byte("def"))
	}
	if err == nil {
		err = writer.Close()
	}
	h.ready <- err
}

func (*streamHandler) OnMessage(*Conn, Message)  {}
func (*streamHandler) OnClose(*Conn, CloseEvent) {}

func (h *compressedStreamHandler) OnOpen(conn *Conn) {
	writer, err := conn.BeginMessage(BinaryMessage)
	if err == nil {
		payload := bytes.Repeat([]byte("compressed-stream-"), 32)
		split := len(payload) / 2
		_, err = writer.Write(payload[:split])
		if err == nil {
			_, err = writer.Write(payload[split:])
		}
	}
	if err == nil {
		err = writer.Close()
	}
	h.ready <- err
}

func (*compressedStreamHandler) OnMessage(*Conn, Message)  {}
func (*compressedStreamHandler) OnClose(*Conn, CloseEvent) {}

func (h *clientHandler) OnOpen(*Conn) { close(h.open) }

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
