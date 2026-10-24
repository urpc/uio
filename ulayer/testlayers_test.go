package ulayer

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/urpc/uio"
)

// The test layers exercise the contract with no cryptography: a byte
// transform, a layer with a handshake of its own, and a layer whose staging
// is bounded. Each is a layer an application could write.

// xorLayer XORs every byte with a key: ready at once, a stream above and
// below, staging only what a callback left unread.
type xorLayer struct{ key byte }

func (x xorLayer) Handler(next uio.Handler) uio.Handler {
	perConn := Func(func(lower uio.Conn, next uio.Handler) uio.Handler {
		conn := &xorConn{Forward: Forward{Lower: lower}, key: x.key}
		return uio.FuncHandler{
			Open:  func(uio.Conn) { next.OnOpen(conn) },
			Data:  func(uio.Conn) error { conn.pull(); next.OnData(conn); return nil },
			Close: func(_ uio.Conn, err error) { next.OnClose(conn, err) },
		}
	})
	return perConn.Handler(next)
}

type xorConn struct {
	Forward
	key byte

	pending []byte // delivered no further than the application read
}

var _ uio.Conn = (*xorConn)(nil)

// pull takes what the connection below delivered, transformed in place.
func (c *xorConn) pull() {
	chunk := c.Lower.PeekChunk()
	for i := range chunk {
		chunk[i] ^= c.key
	}
	_, _ = c.Lower.Discard(len(chunk))
	c.pending = append(c.pending, chunk...)
}

func (c *xorConn) Peek(b []byte) []byte {
	n := copy(b, c.pending)
	return b[:n]
}

func (c *xorConn) PeekChunk() []byte { return c.pending }

func (c *xorConn) Discard(n int) (int, error) {
	if n > len(c.pending) {
		n = len(c.pending)
	}
	c.pending = c.pending[n:]
	return n, nil
}

func (c *xorConn) Read(b []byte) (int, error) {
	n := copy(b, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *xorConn) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(c.pending)
	c.pending = c.pending[n:]
	return int64(n), err
}

func (c *xorConn) InboundBuffered() int { return len(c.pending) }

func (c *xorConn) ReserveInbound(int) error { return nil }

func (c *xorConn) OutboundBuffered() int { return c.Lower.OutboundBuffered() }

func (c *xorConn) Write(p []byte) (int, error) {
	owned := uio.AcquireBuffer(len(p))
	owned.Write(p)
	xorInto(owned.Bytes(), owned.Bytes(), c.key)
	_, err := c.Lower.WriteOwned(owned)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *xorConn) WriteByte(b byte) error {
	_, err := c.Write([]byte{b})
	return err
}

func (c *xorConn) WriteString(s string) (int, error) { return c.Write([]byte(s)) }

func (c *xorConn) Writev(vec [][]byte) (int, error) {
	total := 0
	for _, segment := range vec {
		n, err := c.Write(segment)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (c *xorConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	size := buffer.Len()
	xorInto(buffer.Bytes(), buffer.Bytes(), c.key)
	_, err := c.Lower.WriteOwned(buffer)
	if err != nil {
		return 0, err
	}
	return size, nil
}

func (c *xorConn) ReserveOutbound(int) ([]byte, error) { return nil, uio.ErrReserveUnsupported }

func (c *xorConn) Flush() error { return c.Lower.Flush() }

func (c *xorConn) YieldRead() error { return c.Lower.YieldRead() }

func (c *xorConn) CloseWith(err error) error { return c.Lower.CloseWith(err) }

func (c *xorConn) Close() error { return c.Lower.Close() }

func (c *xorConn) CloseWrite() error { return c.Lower.CloseWrite() }

func xorInto(dst, src []byte, key byte) {
	for i := range src {
		dst[i] = src[i] ^ key
	}
}

// gateLayer runs a handshake before its connection is settled: both sides
// send HELLO?, answer the peer's HELLO? with HELLO!, and settle on HELLO!.
// EarlyOpen hands the connection above over before the handshake, holding
// what it sends until the handshake settles; Refuse answers HELLO? with BYE
// and ends the connection instead; Stall never answers at all.
type gateLayer struct {
	EarlyOpen  bool
	MaxPending int
	Timeout    time.Duration
	Refuse     bool
	Stall      bool

	conns []*gateConn // every connection it served, for the tests
	mu    sync.Mutex
}

var (
	errGateRefused = errors.New("layer: handshake refused")
	errGatePending = errors.New("layer: too much held before the handshake settled")
)

func (g *gateLayer) Handler(next uio.Handler) uio.Handler {
	perConn := Func(func(lower uio.Conn, next uio.Handler) uio.Handler {
		conn := &gateConn{Forward: Forward{Lower: lower}, gate: g, next: next}
		g.mu.Lock()
		g.conns = append(g.conns, conn)
		g.mu.Unlock()
		return uio.FuncHandler{
			Open:  func(uio.Conn) { conn.open() },
			Data:  func(uio.Conn) error { conn.data(); return nil },
			Close: func(_ uio.Conn, err error) { conn.closed(err) },
		}
	})
	return perConn.Handler(next)
}

type gateConn struct {
	Forward
	gate *gateLayer
	next uio.Handler

	mu      sync.Mutex
	pending []byte   // delivered no further than the application read
	held    [][]byte // written above before the handshake settled
	ready   bool
}

var _ uio.Conn = (*gateConn)(nil)

func (c *gateConn) open() {
	c.mu.Lock()
	early := c.gate.EarlyOpen && !c.ready
	c.mu.Unlock()
	if _, err := c.Lower.Write([]byte("HELLO?")); err != nil {
		return // the close follows
	}
	if early {
		c.next.OnOpen(c)
	}
}

func (c *gateConn) data() {
	var held [][]byte
	openNow := false
	c.mu.Lock()
	chunk := c.Lower.PeekChunk()
	c.pending = append(c.pending, chunk...)
	_, _ = c.Lower.Discard(len(chunk))
	if !c.gate.Stall {
		for len(c.pending) > 0 && !c.ready {
			if hasPrefix(c.pending, "HELLO?") {
				c.pending = c.pending[len("HELLO?"):]
				if c.gate.Refuse {
					c.mu.Unlock()
					_, _ = c.Lower.Write([]byte("BYE"))
					_ = c.Lower.CloseWith(errGateRefused)
					return
				}
				_, _ = c.Lower.Write([]byte("HELLO!"))
				continue
			}
			if hasPrefix(c.pending, "HELLO!") {
				c.pending = c.pending[len("HELLO!"):]
				if !c.ready {
					c.ready = true
					held, c.held = c.held, nil
					openNow = !c.gate.EarlyOpen
				}
				continue
			}
			break // the handshake is only partway here
		}
	}
	ready := c.ready
	pending := c.pending
	c.mu.Unlock()
	for _, write := range held { // out the door before the open above
		_, _ = c.Lower.Write(write)
	}
	if openNow {
		c.next.OnOpen(c)
	}
	if ready && len(pending) > 0 {
		c.next.OnData(c)
	}
}

func (c *gateConn) closed(err error) {
	c.next.OnClose(c, err) // a connection above never saw still reports its close
}

func hasPrefix(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == prefix
}

func (c *gateConn) Peek(b []byte) []byte {
	n := copy(b, c.pending)
	return b[:n]
}

func (c *gateConn) PeekChunk() []byte { return c.pending }

func (c *gateConn) Discard(n int) (int, error) {
	if n > len(c.pending) {
		n = len(c.pending)
	}
	c.pending = c.pending[n:]
	return n, nil
}

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

func (c *gateConn) InboundBuffered() int { return len(c.pending) }

func (c *gateConn) ReserveInbound(int) error { return nil }

func (c *gateConn) OutboundBuffered() int {
	c.mu.Lock()
	held := 0
	for _, write := range c.held {
		held += len(write)
	}
	c.mu.Unlock()
	return c.Lower.OutboundBuffered() + held
}

func (c *gateConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if !c.ready {
		held := 0
		for _, write := range c.held {
			held += len(write)
		}
		if c.gate.MaxPending > 0 && held+len(p) > c.gate.MaxPending {
			c.mu.Unlock()
			_ = c.Lower.CloseWith(errGatePending)
			return 0, errGatePending
		}
		c.held = append(c.held, append([]byte(nil), p...))
		c.mu.Unlock()
		return len(p), nil
	}
	c.mu.Unlock()
	return c.Lower.Write(p)
}

func (c *gateConn) WriteByte(b byte) error {
	_, err := c.Write([]byte{b})
	return err
}

func (c *gateConn) WriteString(s string) (int, error) { return c.Write([]byte(s)) }

func (c *gateConn) Writev(vec [][]byte) (int, error) {
	total := 0
	for _, segment := range vec {
		n, err := c.Write(segment)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (c *gateConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	size := buffer.Len()
	data := buffer.Bytes()
	if _, err := c.Write(data); err != nil {
		uio.ReleaseBuffer(buffer)
		return 0, err
	}
	uio.ReleaseBuffer(buffer)
	return size, nil
}

func (c *gateConn) ReserveOutbound(int) ([]byte, error) { return nil, uio.ErrReserveUnsupported }

func (c *gateConn) Flush() error { return c.Lower.Flush() }

func (c *gateConn) YieldRead() error { return c.Lower.YieldRead() }

func (c *gateConn) CloseWith(err error) error { return c.Lower.CloseWith(err) }

func (c *gateConn) Close() error { return c.Lower.Close() }

func (c *gateConn) CloseWrite() error { return c.Lower.CloseWrite() }

// holdLayer stages what it reads below and hands it to the layer above only
// as that layer reads: once its staging reaches capacity it stops consuming
// the connection below, whose unread bytes then hold the socket reads.
type holdLayer struct {
	Capacity int

	conns []*holdConn
	mu    sync.Mutex
}

func (h *holdLayer) Handler(next uio.Handler) uio.Handler {
	perConn := Func(func(lower uio.Conn, next uio.Handler) uio.Handler {
		conn := &holdConn{Forward: Forward{Lower: lower}, hold: h, next: next}
		h.mu.Lock()
		h.conns = append(h.conns, conn)
		h.mu.Unlock()
		return uio.FuncHandler{
			Open:  func(uio.Conn) { next.OnOpen(conn) },
			Data:  func(uio.Conn) error { conn.pump(); next.OnData(conn); return nil },
			Close: func(_ uio.Conn, err error) { next.OnClose(conn, err) },
		}
	})
	return perConn.Handler(next)
}

type holdConn struct {
	Forward
	hold *holdLayer
	next uio.Handler

	mu      sync.Mutex
	pending []byte
}

var _ uio.Conn = (*holdConn)(nil)

// pump takes what the connection below delivered, while there is room.
func (c *holdConn) pump() {
	c.mu.Lock()
	for len(c.pending) < c.hold.Capacity {
		chunk := c.Lower.PeekChunk()
		if len(chunk) == 0 {
			break
		}
		c.pending = append(c.pending, chunk...)
		_, _ = c.Lower.Discard(len(chunk))
	}
	c.mu.Unlock()
}

// snapshot reports what the layer holds and what it left below, for a test
// reading from outside the connection's callbacks.
func (c *holdConn) snapshot() (staged, below int) {
	c.mu.Lock()
	staged = len(c.pending)
	c.mu.Unlock()
	below = c.Lower.InboundBuffered()
	return staged, below
}

func (c *holdConn) Peek(b []byte) []byte {
	n := copy(b, c.pending)
	return b[:n]
}

func (c *holdConn) PeekChunk() []byte { return c.pending }

func (c *holdConn) Discard(n int) (int, error) {
	if n > len(c.pending) {
		n = len(c.pending)
	}
	c.pending = c.pending[n:]
	drained := len(c.pending) < c.hold.Capacity
	below := c.Lower.InboundBuffered()
	if drained && below > 0 {
		// Room again: a Wake runs the round that consumes what is left
		// unread below, and the socket reads with it.
		_ = c.Lower.Wake()
	}
	return n, nil
}

func (c *holdConn) Read(b []byte) (int, error) {
	n := copy(b, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *holdConn) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(c.pending)
	c.pending = c.pending[n:]
	return int64(n), err
}

func (c *holdConn) InboundBuffered() int {
	return len(c.pending) + c.Lower.InboundBuffered()
}

func (c *holdConn) ReserveInbound(int) error { return nil }

func (c *holdConn) OutboundBuffered() int { return c.Lower.OutboundBuffered() }

func (c *holdConn) Write(p []byte) (int, error) { return c.Lower.Write(p) }

func (c *holdConn) WriteByte(b byte) error { return c.Lower.WriteByte(b) }

func (c *holdConn) WriteString(s string) (int, error) { return c.Lower.WriteString(s) }

func (c *holdConn) Writev(vec [][]byte) (int, error) { return c.Lower.Writev(vec) }

func (c *holdConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	return c.Lower.WriteOwned(buffer)
}

func (c *holdConn) ReserveOutbound(n int) ([]byte, error) {
	return c.Lower.ReserveOutbound(n)
}

func (c *holdConn) Flush() error { return c.Lower.Flush() }

func (c *holdConn) YieldRead() error { return c.Lower.YieldRead() }

func (c *holdConn) CloseWith(err error) error { return c.Lower.CloseWith(err) }

func (c *holdConn) Close() error { return c.Lower.Close() }

func (c *holdConn) CloseWrite() error { return c.Lower.CloseWrite() }

// datagramLayer declares a datagram below, to check the kind rules.
type datagramLayer struct{}

func (datagramLayer) Handler(next uio.Handler) uio.Handler {
	perConn := Func(func(lower uio.Conn, next uio.Handler) uio.Handler {
		return uio.FuncHandler{
			Open:  func(uio.Conn) { next.OnOpen(lower) },
			Data:  func(c uio.Conn) error { return next.OnData(c) },
			Close: func(c uio.Conn, err error) { next.OnClose(c, err) },
		}
	})
	return perConn.Handler(next)
}

func (datagramLayer) Requires() Kind { return DatagramKind }
