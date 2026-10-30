package umux

import (
	"errors"
	"testing"
	"time"

	"github.com/urpc/uio"
)

func TestKeepAliveMatchesOutstandingPing(t *testing.T) {
	s, _, _ := quietPair()
	s.pingSent = time.Now()
	s.pingSeq = 42
	for _, h := range []frameHeader{
		{typ: typePing, flags: flagACK, length: 41},
		{typ: typePing, length: 42},
	} {
		if err := s.handlePing(h); err != nil {
			t.Fatal(err)
		}
		if s.pingSent.IsZero() {
			t.Fatal("unrelated response satisfied the pending keepalive")
		}
	}
	if err := s.handlePing(frameHeader{typ: typePing, flags: flagACK, length: 42}); err != nil {
		t.Fatal(err)
	}
	if !s.pingSent.IsZero() {
		t.Fatal("matching ACK did not satisfy the pending keepalive")
	}
}

type productionRejectWire struct {
	productionSessionWire
	writeErr error
}

func (w *productionRejectWire) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.productionSessionWire.Write(p)
}

func TestUnacceptedInboundDoesNotDeliverUnmatchedClose(t *testing.T) {
	for _, mode := range []string{"shutdown-during-accept", "ack-write-failure"} {
		t.Run(mode, func(t *testing.T) {
			closed := 0
			boom := errors.New(mode)
			wire := &productionRejectWire{}
			cfg := Config{KeepAlive: -1, Handler: uio.FuncHandler{Close: func(uio.Conn, error) { closed++ }}}
			var s *Session
			if mode == "shutdown-during-accept" {
				cfg.Accept = func() error { return s.Close(boom) }
			} else {
				wire.writeErr = boom
			}
			s = newSession(wire, cfg.resolve(), false)
			_ = s.handleSyn(frameHeader{typ: typeWindowUpdate, flags: flagSYN, streamID: 1})
			s.onTransportClose(boom)
			if closed != 0 {
				t.Fatalf("delivered %d closes without a preceding accepted OnOpen", closed)
			}
		})
	}
}

func TestDuplicateAckCannotRestoreSpentCredit(t *testing.T) {
	s, st, _ := quietPair()
	st.mu.Lock()
	st.sendWin.take(123)
	want := st.sendWin.available()
	st.mu.Unlock()
	if err := s.handleAck(frameHeader{typ: typeWindowUpdate, flags: flagACK, streamID: st.id}); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	got := st.sendWin.available()
	st.mu.Unlock()
	if got != want {
		t.Fatalf("duplicate ACK restored send credit: got %d, want %d", got, want)
	}
}
