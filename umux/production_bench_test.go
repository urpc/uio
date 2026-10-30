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
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// Run the loopback benchmarks with:
//
//	go test ./umux -run '^$' -bench '^BenchmarkProduction' -benchmem -count=5
//
// TestProductionMixedLoad is short by default. For a sustained run, use:
//
//	UIO_UMUX_STRESS_DURATION=30m UIO_UMUX_STRESS_WORKERS=32 \
//	  go test ./umux -run '^TestProductionMixedLoad$' -timeout 35m -count=1
//
// Every wait below is driven by stream callbacks or context cancellation;
// throughput is not paced by polling sleeps. Listeners keep their assigned
// loopback port open, so setup does not race another process for a free port.

type productionStreamState struct {
	readable chan struct{}
	writable chan struct{}
	done     chan struct{}
	once     sync.Once
	err      error // published by closing done
}

func (s *productionStreamState) signal() {
	select {
	case s.readable <- struct{}{}:
	default:
	}
	select {
	case s.writable <- struct{}{}:
	default:
	}
}

func (s *productionStreamState) wait(ctx context.Context, writing bool) error {
	changed := s.readable
	if writing {
		changed = s.writable
	}
	select {
	case <-changed:
		return nil
	case <-s.done:
		return nil // the next Read or Write reports the close cause
	case <-ctx.Done():
		return ctx.Err()
	}
}

// One endpoint is both handler and observer: a window grant wakes a writer
// just as an arrival wakes a reader. The initialization lock prevents an ACK
// racing the caller's first access from creating two notification channels.
// State lives on the stream, so the harness retains no history of old streams.
type productionEndpoint struct {
	initMu    sync.Mutex
	echo      bool
	lifecycle chan struct{}
	overflows atomic.Int64
}

func (h *productionEndpoint) state(c uio.Conn) *productionStreamState {
	h.initMu.Lock()
	defer h.initMu.Unlock()
	if st, ok := c.Userdata().(*productionStreamState); ok {
		return st
	}
	st := &productionStreamState{readable: make(chan struct{}, 1), writable: make(chan struct{}, 1), done: make(chan struct{})}
	c.SetUserdata(st)
	return st
}

func (h *productionEndpoint) signal(c uio.Conn) {
	if _, ok := c.(*Stream); ok { // transport observations are not stream wakes
		h.state(c).signal()
	}
}

func (h *productionEndpoint) OnOpen(c uio.Conn) { h.state(c) }

func (h *productionEndpoint) OnRead(c uio.Conn) error {
	if !h.echo {
		h.signal(c)
		return nil
	}
	// WriteTo leaves an unaccepted suffix inbound when send credit runs out.
	// A window update dispatches OnRead again, so no echo bytes are dropped.
	if _, err := c.WriteTo(c); err != nil {
		if errors.Is(err, uio.ErrOutboundOverflow) {
			return nil
		}
		return err
	}
	if c.State()&uio.StatePeerEnded != 0 {
		return c.CloseWrite()
	}
	return nil
}

func (*productionEndpoint) OnTimer(uio.Conn) error { return nil }

func (h *productionEndpoint) OnClose(c uio.Conn, err error) {
	st := h.state(c)
	st.once.Do(func() { st.err = err; close(st.done) })
	select {
	case h.lifecycle <- struct{}{}:
	default:
	}
}

func (*productionEndpoint) OnBytesRead(uio.Conn, int)          {}
func (h *productionEndpoint) OnBytesWritten(c uio.Conn, _ int) { h.signal(c) }
func (h *productionEndpoint) OnQueueDrained(c uio.Conn)        { h.signal(c) }

type productionPair struct {
	local, remote  *Session
	client, server *productionEndpoint
	cli            *Client
	srv            *Server
}

func newProductionPair(tb testing.TB, window int) *productionPair {
	tb.Helper()
	p := &productionPair{
		client: &productionEndpoint{lifecycle: make(chan struct{}, 1)},
		server: &productionEndpoint{echo: true, lifecycle: make(chan struct{}, 1)},
	}
	p.srv = NewServer(Config{Handler: p.server, Observer: p.server, WindowSize: window, KeepAlive: -1})
	tb.Cleanup(func() { _ = p.srv.Close(nil); _ = p.srv.events.Wait() })
	ln, err := p.srv.Listen("tcp://127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	p.cli = NewClient(Config{Handler: p.client, Observer: p.client, WindowSize: window, KeepAlive: -1})
	tb.Cleanup(func() { _ = p.cli.Close(nil); _ = p.cli.events.Wait() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.local, err = p.cli.Dial(ctx, "tcp://"+ln.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	select {
	case p.remote = <-p.srv.Sessions():
	case <-ctx.Done():
		tb.Fatal("server session:", ctx.Err())
	}
	return p
}

func productionWriteAll(ctx context.Context, st *Stream, state *productionStreamState, p []byte, overflows *atomic.Int64) error {
	for len(p) > 0 {
		n, err := st.Write(p)
		p = p[n:]
		if err != nil && !errors.Is(err, uio.ErrOutboundOverflow) {
			return err
		}
		if len(p) == 0 {
			return nil
		}
		if overflows != nil {
			overflows.Add(1)
		}
		if err := state.wait(ctx, true); err != nil {
			return err
		}
	}
	return nil
}

func productionReadFull(ctx context.Context, st *Stream, state *productionStreamState, p []byte) error {
	for len(p) > 0 {
		n, err := st.Read(p)
		p = p[n:]
		if len(p) == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			if err := state.wait(ctx, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func productionReadEOF(ctx context.Context, st *Stream, state *productionStreamState) error {
	var extra [1]byte
	for {
		n, err := st.Read(extra[:])
		if n != 0 {
			return errors.New("unexpected payload after the verified echo")
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := state.wait(ctx, false); err != nil {
			return err
		}
	}
}

func productionWaitClosed(ctx context.Context, st *Stream, state *productionStreamState) error {
	select {
	case <-state.done:
		if st.State()&uio.StateClosed == 0 {
			return errors.New("OnClose delivered before StateClosed")
		}
		if st.InboundBuffered() != 0 || st.OutboundBuffered() != 0 {
			return errors.New("closed stream retained buffered payload")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func productionWaitClean(ctx context.Context, st *Stream, state *productionStreamState) error {
	if err := productionWaitClosed(ctx, st, state); err != nil {
		return err
	}
	if state.err != nil && !errors.Is(state.err, io.EOF) {
		return fmt.Errorf("graceful stream ended with %w", state.err)
	}
	return nil
}

func (p *productionPair) peer(st *Stream) (*Stream, *productionStreamState, error) {
	p.remote.mu.Lock()
	peer := p.remote.streams[st.ID()]
	p.remote.mu.Unlock()
	if peer == nil {
		return nil, nil, errors.New("accepted stream missing after its ACK")
	}
	return peer, p.server.state(peer), nil
}

// Both directions' callbacks must settle, not merely return the expected
// payload. This also detects unread receive budget or an abandoned Open.
func (p *productionPair) waitSettled(ctx context.Context) error {
	for {
		settled := true
		for _, sess := range []*Session{p.local, p.remote} {
			sess.mu.Lock()
			if len(sess.streams) != 0 || len(sess.openWait) != 0 || sess.pendingBytes != 0 {
				settled = false
			}
			sess.mu.Unlock()
		}
		if settled {
			return nil
		}
		select {
		case <-p.client.lifecycle:
		case <-p.server.lifecycle:
		case <-ctx.Done():
			return fmt.Errorf("session did not settle: %w", ctx.Err())
		}
	}
}

func BenchmarkProductionRoundTrip(b *testing.B) {
	for _, size := range []int{32, 32 << 10} {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			pair := newProductionPair(b, minWindow)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			st, err := pair.local.Open(ctx)
			if err != nil {
				b.Fatal(err)
			}
			state := pair.client.state(st)
			payload, reply := bytes.Repeat([]byte{0x5a}, size), make([]byte, size)
			b.ReportAllocs()
			b.SetBytes(int64(2 * size)) // request and echoed response payload
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := productionWriteAll(ctx, st, state, payload, nil); err != nil {
					b.Fatal(err)
				}
				if err := productionReadFull(ctx, st, state, reply); err != nil {
					b.Fatal(err)
				}
				if !bytes.Equal(payload, reply) {
					b.Fatal("echo payload changed")
				}
			}
			b.StopTimer()
			if err := st.CloseWrite(); err != nil {
				b.Fatal(err)
			}
			if err := productionReadEOF(ctx, st, state); err != nil {
				b.Fatal(err)
			}
			if err := pair.waitSettled(ctx); err != nil {
				b.Fatal(err)
			}
		})
	}
}

func BenchmarkProductionOpenClose(b *testing.B) {
	pair := newProductionPair(b, minWindow)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := pair.local.Open(ctx)
		if err != nil {
			b.Fatal(err)
		}
		state := pair.client.state(st)
		if err := st.CloseWrite(); err != nil {
			b.Fatal(err)
		}
		if err := productionWaitClosed(ctx, st, state); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := pair.waitSettled(ctx); err != nil {
		b.Fatal(err)
	}
}

func productionMixedStream(ctx context.Context, pair *productionPair, worker, round int) error {
	st, err := pair.local.Open(ctx)
	if err != nil {
		return err
	}
	state := pair.client.state(st)
	peer, peerState, err := pair.peer(st)
	if err != nil {
		return err
	}
	pattern := (worker + round) % 4
	size := []int{32, 32 << 10, 32 << 10, 2*minWindow + 123}[pattern]
	payload := make([]byte, size)
	seed := uint64(worker+1)<<32 | uint64(round+1)
	for i := range payload {
		// Different frames must differ too: a 256-byte repeating pattern
		// would miss duplicated or reordered 16 KiB frame payloads.
		seed = seed*6364136223846793005 + 1442695040888963407
		payload[i] = byte(seed >> 56)
	}
	if pattern == 0 {
		var reply [32]byte
		for i := 0; i < 3; i++ {
			if err := productionWriteAll(ctx, st, state, payload, nil); err != nil {
				return err
			}
			if err := productionReadFull(ctx, st, state, reply[:]); err != nil {
				return err
			}
			if !bytes.Equal(payload, reply[:]) {
				return errors.New("reused echo stream payload changed")
			}
		}
		if err := st.CloseGracefully(nil); err != nil {
			return err
		}
		if err := productionReadEOF(ctx, st, state); err != nil {
			return err
		}
		if err := productionWaitClean(ctx, st, state); err != nil {
			return err
		}
		return productionWaitClean(ctx, peer, peerState)
	}
	if pattern == 2 {
		// Verify a prefix arrived before resetting with unread bytes still in
		// flight; both endpoints must report an abort and release their queues.
		if err := productionWriteAll(ctx, st, state, payload, nil); err != nil {
			return err
		}
		var prefix [32]byte
		if err := productionReadFull(ctx, st, state, prefix[:]); err != nil {
			return err
		}
		if !bytes.Equal(prefix[:], payload[:len(prefix)]) {
			return errors.New("reset stream prefix changed")
		}
		cause := errors.New("production mixed load reset")
		_ = st.CloseWith(cause)
		if err := productionWaitClosed(ctx, st, state); err != nil {
			return err
		}
		if err := productionWaitClosed(ctx, peer, peerState); err != nil {
			return err
		}
		if !errors.Is(state.err, cause) || !errors.Is(peerState.err, ErrStreamReset) {
			return fmt.Errorf("reset causes: local=%v remote=%v", state.err, peerState.err)
		}
		return nil
	}
	// A payload larger than the stream window forces short writes. Read the
	// echo concurrently so two full windows cannot deadlock the test driver.
	writeDone := make(chan error, 1)
	go func() {
		if err := productionWriteAll(ctx, st, state, payload, &pair.client.overflows); err != nil {
			writeDone <- err
			return
		}
		writeDone <- st.CloseWrite()
	}()
	reply := make([]byte, size)
	readErr := productionReadFull(ctx, st, state, reply)
	writeErr := <-writeDone
	if readErr != nil {
		return readErr
	}
	if writeErr != nil {
		return writeErr
	}
	if !bytes.Equal(payload, reply) {
		return fmt.Errorf("echo changed for pattern %d (%d bytes)", pattern, size)
	}
	if err := productionReadEOF(ctx, st, state); err != nil {
		return err
	}
	if err := productionWaitClean(ctx, st, state); err != nil {
		return err
	}
	return productionWaitClean(ctx, peer, peerState)
}

func TestProductionMixedLoad(t *testing.T) {
	workers := 8
	if raw := os.Getenv("UIO_UMUX_STRESS_WORKERS"); raw != "" {
		var err error
		workers, err = strconv.Atoi(raw)
		if err != nil || workers < 1 || workers > 256 {
			t.Fatal("UIO_UMUX_STRESS_WORKERS must be between 1 and 256")
		}
	}
	var duration time.Duration
	if raw := os.Getenv("UIO_UMUX_STRESS_DURATION"); raw != "" {
		var err error
		duration, err = time.ParseDuration(raw)
		if err != nil || duration <= 0 {
			t.Fatal("UIO_UMUX_STRESS_DURATION must be a positive duration")
		}
	}
	pair := newProductionPair(t, minWindow)
	ctx, cancel := context.WithTimeout(context.Background(), duration+30*time.Second)
	defer cancel()
	until := time.Now().Add(duration)
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	var completed atomic.Int64
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; ; round++ {
				// Even a very short requested run covers every close pattern.
				if duration == 0 && round == 8 || duration > 0 && round >= 4 && time.Now().After(until) {
					return
				}
				opCtx, opCancel := context.WithTimeout(ctx, 10*time.Second)
				err := productionMixedStream(opCtx, pair, worker, round)
				opCancel()
				if err != nil {
					errCh <- fmt.Errorf("worker %d round %d: %w", worker, round, err)
					cancel()
					return
				}
				completed.Add(1)
			}
		}(worker)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	if pair.client.overflows.Load() == 0 {
		t.Fatal("mixed load never exercised a stream window short write")
	}
	if err := pair.waitSettled(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("verified %d streams across %d workers; %d window short writes", completed.Load(), workers, pair.client.overflows.Load())
}
