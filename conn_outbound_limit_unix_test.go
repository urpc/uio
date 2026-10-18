//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"golang.org/x/sys/unix"
)

// runBidirectionalLimitTransfer pins that outbound backlog never stops reads:
// both peers fill their own backlog past the limit's old pause mark while the
// sockets hold small buffers, exactly the shape that used to leave each side
// waiting for the other to read. Every byte must reach the far OnData.
func runBidirectionalLimitTransfer(t *testing.T, raw [2]net.Conn, limit int) {
	t.Helper()
	total := int64(limit) * 7 / 8 // past the old pause mark, below the limit
	releaseOpen := make(chan struct{})
	var received [2]atomic.Int64
	var conns [2]Conn
	var events [2]*Events
	var done [2]chan error
	for i := range 2 {
		started := make(chan struct{})
		opened := make(chan Conn, 1)
		ev := &Events{Pollers: 1, MaxOutboundBuffered: limit, MaxBufferSize: 16 << 10}
		ev.OnStart = func(*Events) { close(started) }
		ev.OnOpen = func(conn Conn) {
			opened <- conn
			// Hold the open until both sides have their output queued, so
			// neither reads while the other is still filling.
			<-releaseOpen
		}
		ev.OnData = func(conn Conn) error {
			n, _ := conn.Discard(conn.InboundBuffered())
			received[i].Add(int64(n))
			return nil
		}
		events[i] = ev
		done[i] = make(chan error, 1)
		go func() { done[i] <- ev.Serve() }()
		<-started
		conn, err := ev.Adopt(raw[i], nil)
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = conn
		<-opened
	}
	t.Cleanup(func() {
		for _, ev := range events {
			_ = ev.Close(nil)
		}
		for _, ch := range done {
			select {
			case <-ch:
			case <-time.After(3 * time.Second):
				t.Error("Serve did not stop")
			}
		}
	})

	payload := make([]byte, total)
	for _, conn := range conns {
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	close(releaseOpen)

	// The stall detector reports a mutual wait within a second; the deadline
	// only bounds a slow-but-moving transfer, which a pinned-window pair under
	// -race can be.
	last, lastMoved := int64(-1), time.Now()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		a, b := received[0].Load(), received[1].Load()
		if a == total && b == total {
			return
		}
		if a+b != last {
			last, lastMoved = a+b, time.Now()
		}
		if time.Since(lastMoved) > time.Second {
			first, second := conns[0].(*fdConn), conns[1].(*fdConn)
			t.Fatalf("both directions stopped: received %d/%d and %d/%d; pending %d,%d; readStalled %v,%v; writeBlocked %v,%v",
				a, total, b, total, first.pending.Load(), second.pending.Load(),
				first.readStalled(), second.readStalled(), first.writeBlocked(), second.writeBlocked())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bidirectional transfer did not complete")
}

func TestBidirectionalOutboundLimitUnix(t *testing.T) {
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	var raw [2]net.Conn
	for i, fd := range sockets {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, pinnedSocketBuffer)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, pinnedSocketBuffer)
		file := os.NewFile(uintptr(fd), "bidirectional-peer")
		raw[i], err = net.FileConn(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		defer raw[i].Close()
	}
	runBidirectionalLimitTransfer(t, raw, 1<<20)
}

func TestBidirectionalOutboundLimitTCP(t *testing.T) {
	first, second := tcpConnectionPair(t)
	defer first.Close()
	defer second.Close()
	for _, conn := range []net.Conn{first, second} {
		_ = conn.(*net.TCPConn).SetReadBuffer(pinnedSocketBuffer)
		_ = conn.(*net.TCPConn).SetWriteBuffer(pinnedSocketBuffer)
	}
	// A smaller budget than the Unix case keeps the pinned-window transfer
	// bounded under -race on a loaded host; the mechanism is the same: the
	// backlog sits past the old pause mark for the whole transfer.
	runBidirectionalLimitTransfer(t, [2]net.Conn{first, second}, 2<<20)
}

// A write from another goroutine that passed its entry check before a
// CloseWrite completed is still refused when it comes to queue its bytes:
// queueing them after the end of the stream would leave them unsendable.
func TestQueuedWriteAfterCloseWriteIsRefused(t *testing.T) {
	events := &Events{Pollers: 1}
	client, server := serveInboundTest(t, events)
	if err := server.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := readAllWithin(t, client, 2*time.Second); len(got) != 0 {
		t.Fatalf("read %q before the end of stream", got)
	}
	conn := server.(*fdConn)
	if _, err := conn.queueOwnedWrite(bytebuf.CloneBuffer([]byte("late")), 4); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("queueing after CloseWrite = %v, want net.ErrClosed", err)
	}
	if n := conn.OutboundBuffered(); n != 0 {
		t.Fatalf("%d bytes queued after the end of stream", n)
	}
}

// A CloseWrite waiting for output that a full socket holds back schedules no
// turns while it waits: the FIN follows the writable edge that lets the
// output drain, and the peer then reads everything before the end of stream.
func TestCloseWriteWaitsForAFullSocketWithoutSpinning(t *testing.T) {
	executor := &countingExecutor{}
	want := make([]byte, 8<<20)
	for i := range want {
		want[i] = byte(i * 13)
	}
	queued := make(chan error, 1)
	events := &Events{
		Pollers:  1,
		Executor: executor,
		OnOpen: func(c Conn) {
			_, err := c.Write(want)
			if err == nil {
				err = c.CloseWrite()
			}
			queued <- err
		},
	}
	client, _ := serveInboundTest(t, events)
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the peer reads nothing: the socket stays full
	before := executor.turns.Load()
	time.Sleep(100 * time.Millisecond)
	if spun := executor.turns.Load() - before; spun > 10 {
		t.Fatalf("%d turns scheduled in 100ms while CloseWrite waited for a full socket", spun)
	}
	if got := readAllWithin(t, client, 20*time.Second); !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes before the end of stream, want %d intact", len(got), len(want))
	}
}

// A half-open connection that answers its peer's end of stream with
// CloseWrite closes in the turn that read the end of stream, as a connection
// that does not allow half-open does: the CloseWrite costs no turn of its own.
func TestHalfOpenCloseTakesNoExtraTurn(t *testing.T) {
	for _, halfOpen := range []bool{false, true} {
		executor := &countingExecutor{}
		closed := make(chan struct{})
		events := &Events{Pollers: 1, Executor: executor, AllowHalfOpen: halfOpen}
		events.OnData = func(c Conn) error {
			if _, err := c.WriteTo(c); err != nil {
				return err
			}
			if _, err := c.Read(nil); err == io.EOF {
				return c.CloseWrite()
			}
			return nil
		}
		events.OnClose = func(Conn, error) { close(closed) }
		client, _ := serveInboundTest(t, events)
		if _, err := client.Write(make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(client, make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		before := executor.turns.Load()
		_ = client.Close()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatalf("half-open %v: no close after the peer's end of stream", halfOpen)
		}
		time.Sleep(20 * time.Millisecond)
		if turns := executor.turns.Load() - before; turns != 1 {
			t.Fatalf("half-open %v: closing took %d turns, want 1", halfOpen, turns)
		}
	}
}
