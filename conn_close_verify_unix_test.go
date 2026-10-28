//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package uio

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lockedCloser closes from OnRead while holding the lock its own goodbye
// takes: the walk must run on the turn, after the callback returned.
type lockedCloser struct {
	PassThrough
	mu        sync.Mutex
	goodbye   atomic.Int32
	closeOnce sync.Once
}

func (c *lockedCloser) OnOpen(ctx LayerContext) { ctx.Up().Open() }

func (c *lockedCloser) OnRead(ctx LayerContext) error {
	chunk := ctx.Down().PeekChunk()
	if len(chunk) > 0 {
		_, _ = ctx.Down().Discard(len(chunk))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeOnce.Do(func() { _ = ctx.Close(nil) })
	return nil
}

func (c *lockedCloser) CloseGracefully(LayerContext, error) {
	c.mu.Lock() // the lock OnRead held when it asked
	c.goodbye.Add(1)
	time.Sleep(10 * time.Millisecond)
	c.mu.Unlock()
}

// A layer closing from its own callback under a lock is never called back
// there: its goodbye runs on the turn, so the lock it holds stays its own.
func TestSelfCloseIsNotReentrant(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		_, _ = peer.Write([]byte("x"))
		_, _ = io.Copy(io.Discard, peer)
	}()
	layer := &lockedCloser{}
	closed := make(chan struct{}, 1)
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerHandler { return layer }))
		ev.Handler = FuncHandler{Close: func(Conn, error) { close(closed) }}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(layerWait):
		t.Fatal("the connection never closed: the walk re-entered its own callback")
	}
	if got := layer.goodbye.Load(); got != 1 {
		t.Fatalf("the layer's goodbye ran %d times, want 1", got)
	}
	<-peerDone
}

// A graceful close drains in its mode whatever the cause: the cause is
// reported by OnClose, and never re-read to choose the mode at the socket.
func TestCloseModeSurvivesTheWalk(t *testing.T) {
	if stdioBackend {
		t.Skip("native drain")
	}
	cause := errors.New("goaway")
	closed := make(chan error, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	const total = 256 << 10
	var peerGot atomic.Int64
	var peerErr atomic.Pointer[error]
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		buf := make([]byte, 32<<10)
		for {
			n, err := peer.Read(buf)
			peerGot.Add(int64(n))
			if err != nil {
				peerErr.Store(&err)
				return
			}
		}
	}()
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.Use(PassThroughLayer)
		ev.Handler = FuncHandler{Close: func(_ Conn, err error) { closed <- err }}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sent := 0
	payload := make([]byte, 64<<10)
	deadline := time.Now().Add(5 * time.Second)
	for sent < total && time.Now().Before(deadline) {
		n, err := conn.Write(payload)
		sent += n
		if err != nil {
			break
		}
	}
	if sent == 0 {
		t.Fatal("nothing was accepted")
	}
	if err := conn.CloseGracefully(cause); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, cause) {
			t.Fatalf("OnClose reported %v, want the close's cause", err)
		}
		if errors.Is(err, ErrUnflushedData) {
			t.Fatalf("OnClose reported %v: a graceful close drained first", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the close never finished")
	}
	// The peer's reads settle just after the close reaches it.
	deadline = time.Now().Add(2 * time.Second)
	for peerGot.Load() < int64(sent) && time.Now().Before(deadline) && peerErr.Load() == nil {
		time.Sleep(time.Millisecond)
	}
	if got := peerGot.Load(); got != int64(sent) {
		t.Fatalf("the peer read %d bytes of %d (err %v): the drain did not run", got, sent, peerErr.Load())
	}
}

// A callback error aborts with its goodbyes said: the layers below hear the
// cause before the connection ends.
type erroringLayer struct{ PassThrough }

func (erroringLayer) OnOpen(ctx LayerContext) { ctx.Up().Open() }

func (erroringLayer) OnRead(ctx LayerContext) error {
	if len(ctx.Down().PeekChunk()) > 0 {
		return errors.New("bad frame")
	}
	return nil
}

type goodbyeRecorder struct {
	PassThrough
	cause chan error
}

func (g *goodbyeRecorder) CloseGracefully(_ LayerContext, cause error) {
	select {
	case g.cause <- cause:
	default:
	}
}

func TestCallbackErrorSaysGoodbye(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		peer, err := listener.Accept()
		if err == nil {
			_, _ = peer.Write([]byte("junk"))
			time.Sleep(200 * time.Millisecond)
			_ = peer.Close()
		}
	}()
	below := &goodbyeRecorder{cause: make(chan error, 1)}
	closed := make(chan error, 1)
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerHandler { return below }))
		ev.Use(LayerFunc(func(LayerContext) LayerHandler { return erroringLayer{} }))
		ev.Handler = FuncHandler{Close: func(_ Conn, err error) { closed <- err }}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-below.cause:
		if cause == nil || cause.Error() != "bad frame" {
			t.Fatalf("the goodbye heard %v, want the callback's error", cause)
		}
	case <-time.After(layerWait):
		t.Fatal("the layers below never said goodbye after a callback error")
	}
	select {
	case err := <-closed:
		if err == nil || err.Error() != "bad frame" {
			t.Fatalf("OnClose reported %v, want the callback's error", err)
		}
	case <-time.After(layerWait):
		t.Fatal("the connection never closed")
	}
}

// A Handler that closes the connection inside its own OnOpen makes the dial
// report the close, not hand back a connection already closing.
func TestDialReportsCloseFromOnOpen(t *testing.T) {
	addr := overrideListener(t, 1)
	cause := errors.New("nope")
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Use(PassThroughLayer)
		ev.Handler = FuncHandler{Open: func(c Conn) { _ = c.CloseWith(cause) }}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the dial returned a connection its Handler closed")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("DialContext = %v, want the close's cause", err)
	}
}

// A short OnWrite is a short write: the caller never mistakes it for a
// complete one.
type shortWriter struct{ PassThrough }

func (shortWriter) OnOpen(ctx LayerContext) { ctx.Up().Open() }

func (shortWriter) OnWrite(_ LayerContext, p []byte) (int, error) { return 1, nil }

func TestShortOnWriteIsErrShortWrite(t *testing.T) {
	addr := overrideListener(t, 1)
	wrote := make(chan error, 1)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerHandler { return shortWriter{} }))
		ev.Handler = FuncHandler{
			Open:  func(c Conn) { _, err := c.Write([]byte("hello")); wrote <- err },
			Close: func(Conn, error) {},
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := layerWaitFor(t, wrote, "the write"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write = %v, want io.ErrShortWrite", err)
	}
}

// Embedding PassThrough no longer grants CloseWrite; PassThroughLayer, which
// says it passes the half-close down, keeps a transparent stack whole.
type plainEmbedder struct{ PassThrough }

func (plainEmbedder) OnRead(ctx LayerContext) error { return nil }

func TestPassThroughEmbeddingHasNoCloseWrite(t *testing.T) {
	addr := overrideListener(t, 1)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerHandler { return plainEmbedder{} }))
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.CloseWrite(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("CloseWrite = %v, want ErrUnsupported for a layer that only embeds PassThrough", err)
	}
}

// A receipt's OnSent may write up, and that write gets its own turn, as an
// OnWireSent write does.
type sentWriterReceipt struct {
	PassThrough
	tw   TrackedWriter
	done chan struct{}
	once sync.Once
}

func (s *sentWriterReceipt) OnOpen(ctx LayerContext) {
	s.tw, _ = ctx.Down().(TrackedWriter)
	_, _, _ = s.tw.WriteTracked([]byte("tracked"))
	ctx.Up().Open()
}

func (s *sentWriterReceipt) OnSent(ctx LayerContext, _ SendReceipt) {
	s.once.Do(func() {
		_, _ = ctx.Up().Write([]byte("X"))
		close(s.done)
	})
}

func TestReceiptSentWriteReachesTheHandler(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		if peer, err := listener.Accept(); err == nil {
			defer peer.Close()
			_, _ = io.Copy(io.Discard, peer)
		}
	}()
	layer := &sentWriterReceipt{done: make(chan struct{})}
	var read atomic.Int64
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerHandler { return layer }))
		ev.Handler = FuncHandler{Read: func(c Conn) error {
			read.Add(int64(c.InboundBuffered()))
			_, _ = c.Discard(c.InboundBuffered())
			return nil
		}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-layer.done:
	case <-time.After(layerWait):
		t.Fatal("the receipt never reported")
	}
	deadline := time.Now().Add(layerWait)
	for read.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := read.Load(); got != 1 {
		t.Fatalf("the Handler read %d bytes written from OnSent, want 1", got)
	}
}
