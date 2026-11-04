package uws

import (
	"crypto/tls"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

// Regression tests for the read-control review of 2026-11-04: each one
// failed on the tree before its fix.

// delayingExecutor runs every task on its own goroutine, holding the tasks
// submitted before holdUntil until then, and counts them.
type delayingExecutor struct {
	holdUntil atomic.Int64
	submitted atomic.Int64
}

func (e *delayingExecutor) run(task uio.IOTask) {
	e.submitted.Add(1)
	go func() {
		if d := time.Until(time.Unix(0, e.holdUntil.Load())); d > 0 {
			time.Sleep(d)
		}
		task.RunTask()
	}()
}

func (e *delayingExecutor) Submit(task uio.IOTask) bool { e.run(task); return true }
func (e *delayingExecutor) SubmitBatch(tasks []uio.IOTask) int {
	for _, task := range tasks {
		e.run(task)
	}
	return len(tasks)
}

// The heartbeat's timer callback runs before the turn parses what it read.
// A Pong that reached the socket before the Ping's deadline, on a turn that
// runs after it, must still answer the Ping rather than close the peer.
func TestHeartbeatTimerWaitsForAPongAlreadyRead(t *testing.T) {
	externalReadControlCases(t, func(t *testing.T, serverTLS, clientTLS *tls.Config) {
		exec := &delayingExecutor{}
		handler := &externalReadControlHandler{}
		server, addr, done := startTestServer(t, func() *Server {
			return NewServer(Options{Handler: handler, Pollers: 1, Executor: exec, TLSConfig: serverTLS,
				HeartbeatInterval: 100 * time.Millisecond, HeartbeatTimeout: 200 * time.Millisecond})
		})
		t.Cleanup(func() { stopTestServer(t, server, done) })
		peer, reader := dialExternalReadControl(t, addr, clientTLS, nil)
		opcode, payload, err := readFrameWithOpcode(reader)
		if err != nil || opcode != frame.Ping {
			t.Fatalf("first frame = %v %q, %v; want Ping", opcode, payload, err)
		}
		// Hold the server's turns past the deadline; the Pong is in its
		// socket at once.
		exec.holdUntil.Store(time.Now().Add(350 * time.Millisecond).UnixNano())
		pong := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Pong, Masked: true, Payload: payload}, [4]byte{9, 8, 7, 6})
		if _, err := peer.Write(pong); err != nil {
			t.Fatal(err)
		}
		_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
		opcode, payload, err = readFrameWithOpcode(reader)
		if err != nil {
			t.Fatal(err)
		}
		if opcode != frame.Ping {
			t.Fatalf("after a timely Pong the server sent %v %q, want the next Ping", opcode, payload)
		}
	})
}

// An automatic pause that a local close ends is counted as resumed, so the
// paused and resumed counts stay balanced.
func TestStatsCountAPauseEndedByClose(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	queueReplies(raw, conn, 64)
	if !conn.holdsReplies(64) {
		t.Fatal("no automatic pause")
	}
	conn.markClosing()
	drainTo(raw, conn, 0)
	conn.resumeReadsAfterDrain()
	if stats := server.Stats(); stats.ReadsPaused != 1 || stats.ReadsResumed != 1 {
		t.Fatalf("stats = %+v, want one pause and one resume", stats)
	}
}

// A relay pacing its upstream calls ResumeReads after every forward, mostly
// with nothing paused. That must not cost turns or cut read rounds short.
func TestIdleResumeReadsFromOnMessageKeepsReadRounds(t *testing.T) {
	tasks := func(resume bool) int64 {
		exec := &delayingExecutor{}
		const count = 4096
		var seen atomic.Int64
		finished := make(chan struct{})
		handler := &externalReadControlHandler{message: func(c *Conn, _ Message) {
			if resume {
				_ = c.ResumeReads()
			}
			if seen.Add(1) == count {
				close(finished)
			}
		}}
		server, addr, done := startTestServer(t, func() *Server {
			return NewServer(Options{Handler: handler, Pollers: 1, Executor: exec, ReadBufferSize: 16 << 10})
		})
		defer stopTestServer(t, server, done)
		peer, _ := dialExternalReadControl(t, addr, nil, nil)
		time.Sleep(20 * time.Millisecond)
		parts := make([]string, count)
		for i := range parts {
			parts[i] = string(make([]byte, 1000))
		}
		before := exec.submitted.Load()
		if _, err := peer.Write(maskedFrames(parts...)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d messages", seen.Load())
		}
		return exec.submitted.Load() - before
	}
	// Cut at every read, the 4 MiB in 16 KiB reads takes about 260 turns.
	plain, resumed := tasks(false), tasks(true)
	if resumed > 2*plain+64 {
		t.Fatalf("idle resumes took %d turns, %d without them", resumed, plain)
	}
}

// A paused connection keeps the peer's end of stream behind the frames it
// retained: they are delivered after ResumeReads, and only then does the end
// close the connection.
func TestPausedConnectionKeepsPeerEndBehindRetainedFrames(t *testing.T) {
	opened := make(chan *Conn, 1)
	messages := make(chan string, 4)
	closed := make(chan CloseEvent, 1)
	handler := &externalReadControlHandler{
		open: func(c *Conn) { opened <- c },
		message: func(c *Conn, m Message) {
			messages <- string(m.Payload)
			if string(m.Payload) == "one" {
				if err := c.PauseReads(); err != nil {
					t.Error(err)
				}
			}
		},
		closed: func(_ *Conn, e CloseEvent) { closed <- e },
	}
	server, addr, done := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: handler, Pollers: 1})
	})
	t.Cleanup(func() { stopTestServer(t, server, done) })
	peer, _ := dialExternalReadControl(t, addr, nil, nil)
	conn := <-opened
	if _, err := peer.Write(maskedFrames("one")); err != nil {
		t.Fatal(err)
	}
	if got := <-messages; got != "one" {
		t.Fatal(got)
	}
	if _, err := peer.Write(maskedFrames("two")); err != nil {
		t.Fatal(err)
	}
	_ = peer.(*net.TCPConn).CloseWrite()
	select {
	case e := <-closed:
		t.Fatalf("the peer's end closed a paused connection: %+v", e)
	case m := <-messages:
		t.Fatalf("a paused connection delivered %q", m)
	case <-time.After(100 * time.Millisecond):
	}
	if err := conn.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-messages:
		if m != "two" {
			t.Fatalf("delivered %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery after resume")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("no close after resume")
	}
}

// A Pong behind more frames than one read round's budget is parsed by the
// round's continuation; the round that yielded does not expire the Ping.
func TestHeartbeatSurvivesABudgetYieldBeforeThePong(t *testing.T) {
	exec := &delayingExecutor{}
	handler := &externalReadControlHandler{}
	server, addr, done := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: handler, Pollers: 1, Executor: exec,
			HeartbeatInterval: 100 * time.Millisecond, HeartbeatTimeout: 200 * time.Millisecond})
	})
	t.Cleanup(func() { stopTestServer(t, server, done) })
	peer, reader := dialExternalReadControl(t, addr, nil, nil)
	opcode, payload, err := readFrameWithOpcode(reader)
	if err != nil || opcode != frame.Ping {
		t.Fatalf("first frame = %v %q, %v; want Ping", opcode, payload, err)
	}
	exec.holdUntil.Store(time.Now().Add(350 * time.Millisecond).UnixNano())
	parts := make([]string, minFramesPerDataEvent*2+16)
	for i := range parts {
		parts[i] = "."
	}
	pong := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Pong, Masked: true, Payload: payload}, [4]byte{9, 8, 7, 6})
	if _, err := peer.Write(append(maskedFrames(parts...), pong...)); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	opcode, _, err = readFrameWithOpcode(reader)
	if err != nil {
		t.Fatal(err)
	}
	if opcode == frame.Close {
		t.Fatal("the round that yielded expired a Ping its continuation answered")
	}
}
