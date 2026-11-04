package utls

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/urpc/uio"
)

type contractLower struct {
	uio.LayerDown
	local, remote net.Addr
	written       bytes.Buffer
	writeErr      error
	inbound       []byte
}

func (l *contractLower) LocalAddr() net.Addr  { return l.local }
func (l *contractLower) RemoteAddr() net.Addr { return l.remote }
func (l *contractLower) Write(p []byte) (int, error) {
	if l.writeErr != nil {
		return 0, l.writeErr
	}
	return l.written.Write(p)
}
func (l *contractLower) PeekChunk() []byte { return l.inbound }
func (l *contractLower) Discard(n int) (int, error) {
	l.inbound = l.inbound[n:]
	return n, nil
}
func (*contractLower) State() uio.ConnState { return 0 }

type contractUp struct {
	uio.LayerUp
	buffered int
	yielded  bool
}

func (u *contractUp) Buffered() int { return u.buffered }
func (u *contractUp) Yield()        { u.yielded = true }

type contractContext struct {
	uio.LayerContext
	lower *contractLower
	up    *contractUp
	cause error
}

func (c *contractContext) Down() uio.LayerDown { return c.lower }
func (c *contractContext) Up() uio.LayerUp     { return c.up }
func (c *contractContext) Abort(err error) error {
	c.cause = err
	return nil
}

func TestCiphertextTransportPreservesRecordsAcrossPendingAndBorrowedChunks(t *testing.T) {
	record := func(p string) []byte {
		wire := append([]byte{23, 3, 3, 0, 0}, []byte(p)...)
		binary.BigEndian.PutUint16(wire[3:], uint16(len(p)))
		return wire
	}
	first, second := record("first-record"), record("next")
	wire := append(append([]byte(nil), first...), second...)
	for split := 0; split <= len(wire); split++ {
		for _, readSize := range []int{1, 3, len(wire) + 1} {
			var pending []byte
			if split != 0 {
				pending = bytes.Clone(wire[:split])
			}
			c := &conn{pending: pending, live: wire[split:]}
			transport := &transport{c: c}
			var got []byte
			for len(got) < len(wire) {
				p := make([]byte, readSize)
				n, err := transport.Read(p)
				if err != nil || n == 0 {
					t.Fatalf("split=%d size=%d Read=%d,%v", split, readSize, n, err)
				}
				if len(got) < len(first) && len(got)+n > len(first) {
					t.Fatal("one transport read crossed the TLS record boundary")
				}
				got = append(got, p[:n]...)
			}
			if !bytes.Equal(got, wire) || !c.rec.atBoundary() || c.pending != nil {
				t.Fatal("record assembly changed bytes or retained drained ciphertext")
			}
			if n, err := transport.Read(make([]byte, 1)); n != 0 || err != errWouldBlock {
				t.Fatalf("empty live stream=%d,%v, want would block", n, err)
			}
		}
	}
}

func TestCiphertextTransportClosureSuspensionAndDelegation(t *testing.T) {
	lower := &contractLower{local: &net.TCPAddr{Port: 12}, remote: &net.TCPAddr{Port: 34}}
	ctx := &contractContext{lower: lower, up: &contractUp{}}
	c := &conn{ctx: ctx}
	wire := &transport{c: c}
	if wire.LocalAddr() != lower.local || wire.RemoteAddr() != lower.remote {
		t.Fatal("transport changed endpoint addressing")
	}
	for _, set := range []func(time.Time) error{wire.SetDeadline, wire.SetReadDeadline, wire.SetWriteDeadline} {
		if err := set(time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := wire.Write([]byte("record")); n != 6 || err != nil || lower.written.String() != "record" {
		t.Fatalf("transport write=%d,%v", n, err)
	}
	cause := errors.New("underlying writer failed")
	lower.writeErr = cause
	if n, err := wire.Write([]byte("refused")); n != 0 || !errors.Is(err, cause) {
		t.Fatalf("transport write error=%d,%v", n, err)
	}
	c.yield = func(struct{}) bool {
		c.mu.Lock() // Read must release the engine lock while suspended
		c.live = []byte{23, 3, 3, 0, 1, 'x'}
		c.mu.Unlock()
		return true
	}
	if n, err := wire.Read(make([]byte, 6)); n != 6 || err != nil {
		t.Fatalf("resumed transport read=%d,%v", n, err)
	}
	c.yield = func(struct{}) bool { return false }
	if n, err := wire.Read(make([]byte, 1)); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("canceled coroutine read=%d,%v", n, err)
	}
	c.yield, c.lowerEnded = nil, true
	if n, err := wire.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("underlying EOF=%d,%v", n, err)
	}
	if err := wire.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := wire.Read(make([]byte, 1)); n != 0 || err != errClosed {
		t.Fatalf("closed transport read=%d,%v", n, err)
	}
	if n, err := wire.Write([]byte("closed")); n != 0 || err != errClosed {
		t.Fatalf("closed transport write=%d,%v", n, err)
	}
	if errWouldBlock.Error() == "" || !errWouldBlock.(net.Error).Temporary() || errWouldBlock.(net.Error).Timeout() {
		t.Fatal("would-block error did not retain its temporary/non-timeout contract")
	}
}

func TestTLSLayerRejectsUnopenedAndInvalidLifecycleOperations(t *testing.T) {
	ctx := &contractContext{lower: &contractLower{}, up: &contractUp{}}
	c := &conn{ctx: ctx, tlsLayer: &tlsLayer{}}
	if _, err := c.OnWrite(ctx, []byte("not negotiated")); !errors.Is(err, net.ErrClosed) {
		t.Fatal("an unopened TLS engine accepted application output")
	}
	if err := c.CloseWrite(ctx); !errors.Is(err, net.ErrClosed) {
		t.Fatal("an unopened TLS engine accepted CloseWrite")
	}
	c.CloseGracefully(ctx, nil)
	if err := c.OnRead(ctx); err != nil {
		t.Fatal(err)
	}
	c.ready = true
	ctx.up.buffered = DefaultStagingBound
	if err := c.OnRead(ctx); err != nil || !ctx.up.yielded {
		t.Fatal("a full application delivery was not paced")
	}
	if err := c.OnTimer(ctx); err != nil {
		t.Fatal("a ready TLS connection still timed out its handshake")
	}
	c.pending, c.readAt = []byte("consumed-kept"), len("consumed-")
	c.compactLocked()
	if string(c.pending) != "kept" || c.readAt != 0 {
		t.Fatal("ciphertext compaction lost the unread suffix")
	}
	cause := errors.New("invalid TLS layer")
	bad := &conn{tlsLayer: &tlsLayer{err: cause}}
	bad.OnOpen(ctx)
	if !errors.Is(ctx.cause, cause) || bad.tls != nil {
		t.Fatal("invalid layer created an engine instead of aborting")
	}
	// This handshake fails before I/O because client verification has no host.
	ctx.cause = nil
	bad = &conn{tlsLayer: &tlsLayer{client: true, config: &tls.Config{}, handshakeTimeout: -1}}
	bad.OnOpen(ctx)
	if ctx.cause == nil || bad.ready {
		t.Fatal("failed TLS negotiation opened its application layer")
	}
	bad.OnClose(ctx, nil)
}
