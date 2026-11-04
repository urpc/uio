package umux

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

func TestSessionProtocolRejectionsDoNotPublishStreams(t *testing.T) {
	s, st, _, h := productionStreamPair(t)
	for _, frame := range []frameHeader{
		{typ: typeData, flags: flagSYN, streamID: st.id},
		{typ: typeSynMetadata, streamID: 2},
	} {
		if err := s.handleFrame(frame, nil, false); !errors.Is(err, ErrProtocol) {
			t.Fatalf("invalid frame type=%d flags=%d: %v", frame.typ, frame.flags, err)
		}
	}
	for _, id := range []uint32{0, 3} {
		s.client = true // the remote server can open only nonzero even IDs
		if err := s.checkPeerStreamID(id); !errors.Is(err, ErrProtocol) {
			t.Fatalf("invalid server stream ID %d: %v", id, err)
		}
	}
	if err := s.handleFrame(frameHeader{typ: 255}, nil, false); err != nil || !s.goAway {
		t.Fatal("unknown extension did not stop new stream admission")
	}
	if err := s.handleWindowUpdate(frameHeader{streamID: st.id}); err != nil {
		t.Fatal("an empty credit update was not harmless")
	}
	if err := s.handleData(frameHeader{streamID: st.id, flags: flagRST}, nil, false); err != nil {
		t.Fatal(err)
	}
	s.drainCloses()
	if closed := h.closed(); len(closed) != 1 || !errors.Is(closed[0], ErrStreamReset) {
		t.Fatalf("data-frame reset closes = %v", closed)
	}
	if len(s.streams) != 0 {
		t.Fatal("reset stream remained published")
	}
	s.closed = true
	if err := s.acceptStream(2, 0, nil); err != nil || len(s.streams) != 0 {
		t.Fatal("late SYN published a stream after transport shutdown")
	}
}

func TestSessionShutdownCallsShareOneDrainAndPreserveItsResult(t *testing.T) {
	s, st, _, _ := productionStreamPair(t)
	results := make(chan error, 2)
	var started sync.WaitGroup
	for range 2 {
		started.Add(1)
		go func() {
			started.Done()
			results <- s.Shutdown(nil)
		}()
	}
	started.Wait()
	waitFor(t, "shutdown admission barrier", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.draining
	})
	s.abortStream(st, ErrStreamReset)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("shared graceful shutdown = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("one of the shutdown callers missed the drain completion")
		}
	}
	s.forceDrain(context.Canceled) // a late cancellation cannot change success
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("completed drain lost its result: %v", err)
	}
}

func TestSessionShutdownReportsBrokenTransportAndAlreadyClosedCause(t *testing.T) {
	cause := errors.New("test: GoAway write failed")
	wire := &productionRejectWire{writeErr: cause}
	s := newSession(wire, Config{KeepAlive: -1}.resolve(), true)
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[st.id] = st
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- s.Shutdown(ctx) }()
	waitFor(t, "broken transport close", wire.closed.Load)
	s.onTransportClose(cause)
	if err := <-result; !errors.Is(err, cause) {
		t.Fatalf("shutdown = %v, want broken transport cause", err)
	}
	closed := newSession(&productionSessionWire{}, Config{KeepAlive: -1}.resolve(), true)
	closed.onTransportClose(cause)
	if err := closed.Shutdown(nil); !errors.Is(err, cause) {
		t.Fatalf("shutdown after transport failure = %v", err)
	}
}

func TestSessionRendezvousAndAnnouncementAfterServerClose(t *testing.T) {
	wire := &productionSessionWire{}
	s := newSession(wire, Config{KeepAlive: -1}.resolve(), true)
	r := &router{conns: map[uio.Conn]*Session{wire: s}}
	if got, err := r.waitSession(nil, wire); err != nil || got != s {
		t.Fatal("completed session rendezvous did not return its existing session")
	}
	r.OnStop(nil)
	server := NewServer(Config{KeepAlive: -1})
	server.closeSessions()
	server.publishSession(s)
	if !wire.closed.Load() {
		t.Fatal("a session announced after shutdown was left live")
	}
	if _, ok := <-server.Sessions(); ok {
		t.Fatal("server shutdown reopened its announcement queue")
	}
}
