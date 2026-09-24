package uws

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHandshakeResourcesDetachExactlyOnce(t *testing.T) {
	for _, action := range []string{"open", "stop", "expire"} {
		t.Run(action, func(t *testing.T) {
			var contextStops, cleanups atomic.Int32
			state := &handshakeState{
				epoch:       1,
				timer:       time.NewTimer(time.Hour),
				contextStop: func() bool { contextStops.Add(1); return true },
				cleanup:     func() { cleanups.Add(1) },
			}
			conn := &Conn{raw: newScriptedConn()}
			conn.handshake.Store(state)
			switch action {
			case "open":
				if !conn.markOpened() {
					t.Fatal("markOpened rejected live handshake")
				}
				if conn.handshake.Load() != state {
					t.Fatal("OnOpen lost handshake state before callback")
				}
				conn.notifyOpen()
			case "stop":
				conn.stopHandshakeTimer()
			case "expire":
				conn.expireHandshake(state, 1, context.DeadlineExceeded)
			}
			conn.stopHandshakeTimer()
			conn.expireHandshake(state, 1, context.DeadlineExceeded)
			if got := contextStops.Load(); got != 1 {
				t.Fatalf("context stops = %d, want 1", got)
			}
			if got := cleanups.Load(); got != 1 {
				t.Fatalf("cleanups = %d, want 1", got)
			}
			if conn.handshake.Load() != nil {
				t.Fatal("handshake state retained after completion")
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.timer != nil || state.contextStop != nil || state.cleanup != nil {
				t.Fatal("handshake resources retained after completion")
			}
		})
	}
}

func TestHandshakeOpenStopAndExpiryRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		var cleanups atomic.Int32
		state := &handshakeState{epoch: 1, cleanup: func() { cleanups.Add(1) }}
		raw := newScriptedConn()
		conn := &Conn{raw: raw}
		conn.handshake.Store(state)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); conn.markOpened() }()
		go func() { defer wg.Done(); conn.stopHandshakeTimer() }()
		go func() { defer wg.Done(); conn.expireHandshake(state, 1, context.DeadlineExceeded) }()
		wg.Wait()
		if got := cleanups.Load(); got != 1 {
			t.Fatalf("iteration %d: cleanups = %d, want 1", i, got)
		}
		if conn.handshake.Load() != nil {
			t.Fatalf("iteration %d: handshake state retained", i)
		}
		if conn.opened.Load() && raw.closes != 0 {
			t.Fatalf("iteration %d: opened connection expired", i)
		}
	}
}
