package uws

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

func waitSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func TestExternalCloseInSingleFrameRound(t *testing.T) {
	raw := &limitedWire{limit: 1 << 20}
	c := newLimitedConn(raw, time.Hour)
	t.Cleanup(c.stopCloseTimer)
	entered, resume := make(chan struct{}), make(chan struct{})
	c.handler = &batchEchoHandler{onMessage: func(*Conn, Message) {
		close(entered)
		<-resume
	}}
	raw.inbound = maskedFrames("one")
	done := make(chan error, 1)
	go func() { done <- c.readAvailable() }()
	waitSignal(t, entered, "handler")
	closeErr := c.Close(1000, "bye")
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if got := raw.frames(t); len(got) != 1 || got[0].Opcode != frame.Close {
		t.Fatalf("round returned without sending Close: frames=%v deferred=%v", got, c.writes.close.closeIsDeferred())
	}
}

func TestExternalSendCannotStealBatchRoom(t *testing.T) {
	raw := &limitedWire{limit: 64}
	c := newLimitedConn(raw, time.Hour)
	entered, resume := make(chan struct{}), make(chan struct{})
	accepted := make(chan error, 1)
	first := true
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, _ Message) {
		if !first {
			return
		}
		first = false
		accepted <- c.SendBinary(bytes.Repeat([]byte("a"), 40))
		close(entered)
		<-resume
	}}
	raw.inbound = maskedFrames("one", "two")
	done := make(chan error, 1)
	go func() { done <- c.readAvailable() }()
	waitSignal(t, entered, "accepted batch")
	batchErr := <-accepted
	externalErr := c.SendBinary(bytes.Repeat([]byte("b"), 40))
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if batchErr != nil {
		t.Fatal(batchErr)
	}
	if raw.closeCount() != 0 {
		t.Fatalf("accepted batch lost: external send=%v closes=%d close error=%v delivered=%d", externalErr, raw.closeCount(), c.closeInfo().Err, len(raw.frames(t)))
	}
}

func TestHeartbeatCloseFollowsAcceptedBatch(t *testing.T) {
	raw := &limitedWire{limit: 1 << 20}
	c := newLimitedConn(raw, time.Hour)
	c.heartbeat = &heartbeatState{}
	t.Cleanup(c.stopCloseTimer)
	entered, resume := make(chan struct{}), make(chan struct{})
	accepted := make(chan error, 1)
	first := true
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, _ Message) {
		if !first {
			return
		}
		first = false
		accepted <- c.SendBinary([]byte("accepted"))
		close(entered)
		<-resume
	}}
	raw.inbound = maskedFrames("one", "two")
	done := make(chan error, 1)
	go func() { done <- c.readAvailable() }()
	waitSignal(t, entered, "accepted batch")
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	c.sendHeartbeatClose(1001, "heartbeat timeout")
	close(resume)
	if err := <-done; err != nil && !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	got := raw.frames(t)
	if len(got) != 2 || got[0].Opcode != frame.Binary || got[1].Opcode != frame.Close {
		t.Fatalf("heartbeat Close overtook accepted batch: %v", got)
	}
}

type fragmentGateWire struct {
	limitedWire
	firstFragment   chan struct{}
	releaseFragment chan struct{}
	batchFlushed    chan struct{}
	firstOnce       sync.Once
	batchOnce       sync.Once
}

func (w *fragmentGateWire) WriteOwned(b *uio.Buffer) (int, error) {
	header, size := b.Bytes()[0], b.Len()
	n, err := w.limitedWire.WriteOwned(b)
	if err != nil {
		return n, err
	}
	if header == byte(frame.Binary) {
		w.firstOnce.Do(func() { close(w.firstFragment); <-w.releaseFragment })
	}
	if header == 0x80|byte(frame.Binary) && size > 3 {
		w.batchOnce.Do(func() { close(w.batchFlushed) })
	}
	return n, err
}

func (w *fragmentGateWire) Writev(vec [][]byte) (int, error) {
	header := vec[0][0]
	n, err := w.limitedWire.Writev(vec)
	if err == nil && header == byte(frame.Binary) {
		w.firstOnce.Do(func() { close(w.firstFragment); <-w.releaseFragment })
	}
	return n, err
}

func TestBatchCannotInterruptFragmentedMessage(t *testing.T) {
	raw := &fragmentGateWire{
		limitedWire:   limitedWire{limit: 1 << 20},
		firstFragment: make(chan struct{}), releaseFragment: make(chan struct{}), batchFlushed: make(chan struct{}),
	}
	c := newLimitedConn(&raw.limitedWire, time.Hour)
	c.raw = raw
	c.config = testServerConfig(NewServer(Options{MaxOutboundBuffered: 1 << 20, OutboundHighWatermark: -1, MaxFramePayload: 8, WriteCoalesce: 10}, nil))
	entered, resume := make(chan struct{}), make(chan struct{})
	first := true
	handlerErr := make(chan error, 1)
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, _ Message) {
		if !first {
			return
		}
		first = false
		if err := c.SendBinary([]byte("x")); err != nil {
			handlerErr <- err
			return
		}
		close(entered)
		<-resume
		for i := 0; i < 2000; i++ {
			if err := c.SendBinary([]byte("x")); err != nil {
				handlerErr <- err
				return
			}
		}
		handlerErr <- nil
	}}
	raw.inbound = maskedFrames("one", "two")
	roundDone := make(chan error, 1)
	go func() { roundDone <- c.readAvailable() }()
	waitSignal(t, entered, "accepted round reply")
	externalDone := make(chan error, 1)
	go func() { externalDone <- c.SendBinary([]byte("abcdefghijklmnop")) }()
	waitSignal(t, raw.firstFragment, "external first fragment")
	close(resume)
	select {
	case <-raw.batchFlushed:
	case <-time.After(100 * time.Millisecond):
	}
	close(raw.releaseFragment)
	if err := <-externalDone; err != nil {
		t.Fatal(err)
	}
	if err := <-roundDone; err != nil {
		t.Fatal(err)
	}
	if err := <-handlerErr; err != nil {
		t.Fatal(err)
	}
	inside := false
	for i, f := range raw.frames(t) {
		if f.Opcode == frame.Binary {
			if inside {
				t.Fatalf("new Binary frame interrupted unfinished external message at frame %d: fin=%v payload=%q", i, f.Fin, f.Payload)
			}
			inside = !f.Fin
		} else if f.Opcode == frame.Continuation {
			inside = !f.Fin
		}
	}
}

func TestFragmentAdmissionRacesBatchMutation(t *testing.T) {
	raw := &limitedWire{limit: 100 << 20}
	c := newLimitedConn(raw, time.Hour)
	c.config = testServerConfig(NewServer(Options{MaxOutboundBuffered: 100 << 20, OutboundHighWatermark: -1, MaxFramePayload: 8}, nil))
	start := make(chan struct{})
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, _ Message) {
		<-start
		for i := 0; i < 5000; i++ {
			if err := c.SendBinary([]byte("12345678")); err != nil {
				return
			}
		}
	}}
	raw.inbound = maskedFrames("one", "two")
	done := make(chan error, 1)
	go func() { done <- c.readAvailable() }()
	close(start)
	for i := 0; i < 5000; i++ {
		if err := c.SendBinary([]byte("abcdefghijklmnop")); err != nil {
			t.Fatal(fmt.Errorf("external send: %w", err))
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatBatchRespectsQuota(t *testing.T) {
	raw := &limitedWire{limit: 64}
	c := newLimitedConn(raw, time.Hour)
	c.heartbeat = &heartbeatState{}
	var sends []error
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, m Message) {
		sends = append(sends, c.SendBinary(m.Payload))
	}}
	raw.inbound = maskedFrames(string(bytes.Repeat([]byte("a"), 40)), string(bytes.Repeat([]byte("b"), 40)))
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	held := 0
	if c.batch != nil {
		held = c.batch.Len()
	}
	if len(sends) != 2 || sends[0] != nil || !errors.Is(sends[1], ErrBackpressure) || held != 0 || len(raw.frames(t)) != 1 {
		t.Fatalf("quota failed: sends=%v delivered=%d retained=%d transport=%d pending=%d", sends, len(raw.frames(t)), held, raw.queuedBytes(), c.writes.close.pendingBytes.Load())
	}
}

type snapshotGateWire struct {
	limitedWire
	gate    atomic.Bool
	entered chan struct{}
	resume  chan struct{}
}

func (w *snapshotGateWire) OutboundBuffered() int {
	queued := w.limitedWire.OutboundBuffered()
	if w.gate.CompareAndSwap(false, true) {
		close(w.entered)
		<-w.resume
	}
	return queued
}

func TestQuotaReservationIsAtomic(t *testing.T) {
	raw := &snapshotGateWire{limitedWire: limitedWire{limit: 64}, entered: make(chan struct{}), resume: make(chan struct{})}
	c := newLimitedConn(&raw.limitedWire, time.Hour)
	c.raw = raw
	first := true
	accepted := make(chan error, 1)
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, _ Message) {
		if first {
			first = false
			accepted <- c.SendBinary(bytes.Repeat([]byte("a"), 40))
		}
	}}
	raw.inbound = maskedFrames("one", "two")
	done := make(chan error, 1)
	go func() { done <- c.readAvailable() }()
	waitSignal(t, raw.entered, "round's quota snapshot")
	externalDone := make(chan error, 1)
	go func() { externalDone <- c.SendBinary(bytes.Repeat([]byte("b"), 40)) }()
	var externalErr error
	select {
	case externalErr = <-externalDone:
		close(raw.resume)
	case <-time.After(100 * time.Millisecond):
		// The locked baseline cannot reserve until the round releases its lock.
		close(raw.resume)
		externalErr = <-externalDone
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	batchErr := <-accepted
	held := 0
	if c.batch != nil {
		held = c.batch.Len()
	}
	if raw.queuedBytes()+held > raw.limit {
		t.Fatalf("non-atomic quota: round send=%v external send=%v transport=%d retained=%d limit=%d", batchErr, externalErr, raw.queuedBytes(), held, raw.limit)
	}
}
