package uws

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

type concurrentCloseConn struct {
	*scriptedConn
	mu sync.Mutex
}

func (raw *concurrentCloseConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	raw.mu.Lock()
	defer raw.mu.Unlock()
	return raw.scriptedConn.WriteOwned(buffer)
}

func (raw *concurrentCloseConn) Writev(buffers [][]byte) (int, error) {
	raw.mu.Lock()
	defer raw.mu.Unlock()
	return raw.scriptedConn.Writev(buffers)
}

func (raw *concurrentCloseConn) Flush() error {
	raw.mu.Lock()
	defer raw.mu.Unlock()
	return raw.scriptedConn.Flush()
}

func (raw *concurrentCloseConn) CloseWith(err error) error {
	raw.mu.Lock()
	defer raw.mu.Unlock()
	return raw.scriptedConn.CloseWith(err)
}

type queueCloseDuringWriteConn struct {
	*scriptedConn
	conn   *Conn
	queued bool
}

func (raw *queueCloseDuringWriteConn) queueAnotherClose() {
	if raw.queued {
		return
	}
	raw.queued = true
	raw.conn.writes.close.queueClose([]byte{3, 232})
}

func (raw *queueCloseDuringWriteConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	raw.queueAnotherClose()
	return raw.scriptedConn.WriteOwned(buffer)
}

func (raw *queueCloseDuringWriteConn) Writev(buffers [][]byte) (int, error) {
	raw.queueAnotherClose()
	return raw.scriptedConn.Writev(buffers)
}

func TestCloseProgressDrainsRequestQueuedDuringWrite(t *testing.T) {
	raw := &queueCloseDuringWriteConn{scriptedConn: newScriptedConn()}
	conn := &Conn{raw: raw, config: testServerConfig(NewServer(nil))}
	conn.opened.Store(true)
	raw.conn = conn

	writer, err := conn.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.protocolClose(frame.ErrProtocol); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("protocol close = %v", err)
	}
	if !conn.writes.close.hasPendingClose() {
		t.Fatal("Writer did not inherit the pending Close frame")
	}
	_ = writer.fail(ErrClosed)
	if !raw.queued || conn.writes.close.hasPendingClose() || conn.writes.close.drainIsRequested() {
		t.Fatalf("drain state = queued:%v pending:%v requested:%v", raw.queued, conn.writes.close.hasPendingClose(), conn.writes.close.drainIsRequested())
	}
	if raw.writes != 1 || raw.closes != 0 {
		t.Fatalf("transport before outbound retirement = writes:%d closes:%d", raw.writes, raw.closes)
	}
	completeTestOutbound(conn)
	if raw.closes != 1 || conn.writes.close.phase() != transportCloseClaimed {
		t.Fatalf("transport completion = closes:%d phase:%d", raw.closes, conn.writes.close.phase())
	}
	if err := conn.closeTransport(); err != nil {
		t.Fatal(err)
	}
	if raw.closes != 1 || conn.writes.close.drainIsRequested() {
		t.Fatalf("duplicate close changed completion: closes:%d requested:%v", raw.closes, conn.writes.close.drainIsRequested())
	}
}

func TestRepeatedTransportCloseHasOneOwner(t *testing.T) {
	raw := newScriptedConn()
	conn := testServerConn(raw)
	if err := conn.closeTransport(); err != nil {
		t.Fatal(err)
	}
	if err := conn.closeTransport(); err != nil {
		t.Fatal(err)
	}
	if raw.closes != 1 || conn.writes.close.phase() != transportCloseClaimed {
		t.Fatalf("transport closes = %d, phase = %d", raw.closes, conn.writes.close.phase())
	}
}

func TestWriterFailureRacesProtocolClose(t *testing.T) {
	for i := 0; i < 100; i++ {
		raw := &concurrentCloseConn{scriptedConn: newScriptedConn()}
		conn := &Conn{raw: raw, config: testServerConfig(NewServer(nil))}
		conn.opened.Store(true)
		writer, err := conn.BeginMessage(BinaryMessage)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_ = writer.fail(ErrClosed)
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = conn.protocolClose(frame.ErrProtocol)
		}()
		close(start)
		wg.Wait()
		completeTestOutbound(conn)
		if raw.closes != 1 || conn.writes.close.phase() != transportCloseClaimed {
			t.Fatalf("iteration %d: closes = %d, phase = %d", i, raw.closes, conn.writes.close.phase())
		}
		if conn.writes.close.hasPendingClose() || conn.writes.close.drainIsRequested() {
			t.Fatalf("iteration %d: Close frame handoff was not cleared", i)
		}
	}
}

func TestCloseTimeoutAbortsWriterHandoffOnce(t *testing.T) {
	raw := newScriptedConn()
	raw.closed = make(chan struct{})
	conn := &Conn{
		raw:    raw,
		config: testServerConfig(&Server{CloseTimeout: time.Millisecond}),
	}
	conn.opened.Store(true)
	writer, err := conn.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.protocolClose(frame.ErrProtocol); !errors.Is(err, frame.ErrProtocol) {
		t.Fatal(err)
	}
	select {
	case <-raw.closed:
	case <-time.After(testIOTimeout()):
		t.Fatal("close timeout waited for streaming Writer")
	}
	_ = writer.fail(ErrClosed)
	if raw.closes != 1 || conn.writes.close.phase() != transportCloseClaimed || conn.writes.close.hasPendingClose() {
		t.Fatalf("timeout result = closes:%d phase:%d pending:%v", raw.closes, conn.writes.close.phase(), conn.writes.close.hasPendingClose())
	}
}
