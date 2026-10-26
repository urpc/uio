package utls

import (
	"io"
	"sync"

	"github.com/urpc/uio"
)

// gate is a wrapper that runs a one-round handshake before its connection
// settles: both sides send HELLO?, answer with HELLO!, and settle on the
// answer. It is a wrapper with a handshake under the TLS wrapper.
func gate(next uio.Handler) uio.Handler {
	return uio.PerConn(func(lower uio.Conn, next uio.Handler) uio.Handler {
		conn := &gateConn{InnerConn: uio.InnerConn{Conn: lower}, next: next}
		return uio.FuncHandler{
			Open:  func(uio.Conn) { conn.open() },
			Data:  func(uio.Conn) error { return conn.data() },
			Close: func(_ uio.Conn, err error) { next.OnClose(conn, err) },
		}
	})(next)
}

type gateConn struct {
	uio.InnerConn
	next    uio.Handler
	mu      sync.Mutex
	pending []byte
	ready   bool
}

func (c *gateConn) open() {
	_, _ = c.Conn.Write([]byte("HELLO?"))
}

func (c *gateConn) data() error {
	c.mu.Lock()
	chunk := c.Conn.PeekChunk()
	c.pending = append(c.pending, chunk...)
	_, _ = c.Conn.Discard(len(chunk))
	for !c.ready && len(c.pending) >= len("HELLO?") {
		switch {
		case string(c.pending[:len("HELLO?")]) == "HELLO?":
			c.pending = c.pending[len("HELLO?"):]
			_, _ = c.Conn.Write([]byte("HELLO!"))
		case string(c.pending[:len("HELLO!")]) == "HELLO!":
			c.pending = c.pending[len("HELLO!"):]
			c.ready = true
			c.next.OnOpen(c)
		default:
			c.ready = true // not a gate peer: pass through unsettled
			c.next.OnOpen(c)
		}
	}
	ready, pending := c.ready, c.pending
	c.mu.Unlock()
	if ready && len(pending) > 0 {
		return c.next.OnData(c)
	}
	return nil
}

func (c *gateConn) PeekChunk() []byte    { return c.pending }
func (c *gateConn) Peek(b []byte) []byte { n := copy(b, c.pending); return b[:n] }
func (c *gateConn) Discard(n int) (int, error) {
	if n > len(c.pending) {
		n = len(c.pending)
	}
	c.pending = c.pending[n:]
	return n, nil
}
func (c *gateConn) InboundBuffered() int                  { return len(c.pending) }
func (c *gateConn) ReserveInbound(int) error              { return nil }
func (c *gateConn) OutboundBuffered() int                 { return c.Conn.OutboundBuffered() }
func (c *gateConn) Write(p []byte) (int, error)           { return c.Conn.Write(p) }
func (c *gateConn) Writev(v [][]byte) (int, error)        { return c.Conn.Writev(v) }
func (c *gateConn) WriteOwned(b *uio.Buffer) (int, error) { return c.Conn.WriteOwned(b) }
func (c *gateConn) ReserveOutbound(n int) ([]byte, error) {
	return c.Conn.ReserveOutbound(n)
}
func (c *gateConn) Flush() error { return c.Conn.Flush() }

func (c *gateConn) YieldRead() error          { return c.Conn.YieldRead() }
func (c *gateConn) CloseWith(err error) error { return c.Conn.CloseWith(err) }
func (c *gateConn) Close() error              { return c.Conn.Close() }
func (c *gateConn) CloseWrite() error         { return c.Conn.CloseWrite() }

func (c *gateConn) Read(b []byte) (int, error) {
	n := copy(b, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *gateConn) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(c.pending)
	c.pending = c.pending[n:]
	return int64(n), err
}

func (c *gateConn) WriteByte(b byte) error {
	_, err := c.Conn.Write([]byte{b})
	return err
}

func (c *gateConn) WriteString(s string) (int, error) { return c.Conn.Write([]byte(s)) }
