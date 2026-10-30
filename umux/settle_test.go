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

package umux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// stalledDrain leaves every stream's inbound unconsumed (backpressure) until
// armed, then drains it; it never half-closes on its own.
type stalledDrain struct {
	armed  atomic.Bool
	opened chan *Stream
	closed chan error
}

func (h *stalledDrain) OnOpen(c uio.Conn) {
	select {
	case h.opened <- c.(*Stream):
	default:
	}
}

func (h *stalledDrain) OnRead(c uio.Conn) error {
	if !h.armed.Load() {
		return nil
	}
	st := c.(*Stream)
	buf := make([]byte, 8<<10)
	for {
		n, err := st.Read(buf)
		if n > 0 {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
}

func (h *stalledDrain) OnTimer(uio.Conn) error { return nil }

func (h *stalledDrain) OnClose(_ uio.Conn, err error) {
	select {
	case h.closed <- err:
	default:
	}
}

// TestWakeTurnSettlesStream mirrors the tunnel's stall: the peer's FIN arrives
// while the relay is backed up, so the callback cannot consume everything and
// the stream cannot settle; the drain happens later, in a wake turn. That turn
// must settle the stream like an arrival turn does, or the stream stays
// half-open forever holding the session's budget.
func TestWakeTurnSettlesStream(t *testing.T) {
	h := &stalledDrain{opened: make(chan *Stream, 1), closed: make(chan error, 4)}
	sess, _ := startPair(t, Config{Handler: h}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cl, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.CloseWith(errors.New("done"))
	writeAll(t, cl, bytes.Repeat([]byte("z"), 32<<10))
	if err := cl.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	var srv *Stream
	select {
	case srv = <-h.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the stream")
	}
	waitFor(t, "inbound backed up", func() bool { return srv.InboundBuffered() > 0 })

	// End this end the way the bridge does when the user conn is done; the
	// stream cannot settle while the callback still holds bytes.
	if err := srv.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	h.armed.Store(true)
	if err := srv.Wake(); err != nil {
		t.Fatal(err)
	}
	select {
	case cerr := <-h.closed:
		if !errors.Is(cerr, io.EOF) {
			t.Fatalf("stream closed with %v, want io.EOF", cerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("stream never settled after the wake turn (inbound %d)", srv.InboundBuffered())
	}
}
