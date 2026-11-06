//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"errors"
	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/socket"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestNativeDirectWritesPreservePayloadAndOverflowOwnership(t *testing.T) {
	for _, api := range []string{"Write", "Writev", "WriteOwned"} {
		for _, mode := range []string{"complete", "partial-queued", "partial-overflow", "refused-overflow", "refused-queued", "peer-closed"} {
			t.Run(api+"/"+mode, func(t *testing.T) {
				fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
				if err != nil {
					t.Fatal(err)
				}
				for _, fd := range fds {
					if err := unix.SetNonblock(fd, true); err != nil {
						t.Fatal(err)
					}
				}
				if err := unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 4<<10); err != nil {
					t.Fatal(err)
				}
				filled := 0
				if mode == "refused-overflow" || mode == "refused-queued" {
					filled = fillSocketUntilRefused(t, fds[0])
				}
				replacement := -1
				if mode == "peer-closed" {
					replacement, err = unix.Open("/dev/null", unix.O_RDONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						_ = unix.Close(replacement)
					})
				}
				payload := bytes.Repeat([]byte("scatter-gather!"), 1<<16)
				if mode == "complete" || mode == "peer-closed" {
					payload = payload[:32]
				}
				want := bytes.Clone(payload)
				type result struct {
					n, buffered int
					err         error
					retained    bool
				}
				results := make(chan result, 1)
				closed := make(chan error, 1)
				ev := &Events{Pollers: 1}
				if mode == "partial-overflow" || mode == "refused-overflow" {
					ev.MaxOutboundBuffered = 1
				}
				funcs(ev).Open = func(c Conn) {
					if replacement >= 0 {
						if err := unix.Dup2(replacement, fds[1]); err != nil {
							t.Error(err)
						}
					}
					var n int
					var err error
					retained := false
					switch api {
					case "Write":
						n, err = c.Write(payload)
					case "Writev":
						cut := len(payload) / 2
						n, err = c.Writev([][]byte{nil, payload[:5], payload[5:cut], nil, payload[cut:]})
					case "WriteOwned":
						owned := AcquireBuffer(len(payload))
						_, _ = owned.Write(payload)
						n, err = c.WriteOwned(owned)
						if errors.Is(err, ErrOutboundOverflow) {
							retained = bytes.Equal(owned.Bytes(), want)
							ReleaseBuffer(owned)
						}
					}
					results <- result{n: n, err: err, buffered: c.OutboundBuffered(), retained: retained}
					clear(payload)
				}
				funcs(ev).Close = func(_ Conn, err error) {
					closed <- err
				}
				connection, registered := startTestConnectionOn(t, ev, fds)
				if err := <-registered; err != nil {
					t.Fatal(err)
				}
				got := readControlAwait(t, results, "direct callback write")
				switch mode {
				case "complete", "partial-queued", "refused-queued":
					if got.n != len(want) || got.err != nil {
						t.Fatalf("accepted write=%d,%v, want %d,nil", got.n, got.err, len(want))
					}
					if mode != "complete" && (got.buffered == 0 || got.buffered > len(want)) {
						t.Fatalf("partial send did not retain its suffix: buffered=%d", got.buffered)
					}
					if filled != 0 {
						_ = readPeerUntil(t, connection.peer, filled, 10*time.Second)
					}
					if wire := readPeerUntil(t, connection.peer, len(want), 10*time.Second); !bytes.Equal(wire, want) {
						t.Fatal("queued direct-send suffix retained or reordered caller memory")
					}
				case "partial-overflow":
					if got.n <= 0 || got.n >= len(want) || !errors.Is(got.err, io.ErrShortWrite) || errors.Is(got.err, ErrOutboundOverflow) {
						t.Fatalf("partial write=%d,%v, want positive short write", got.n, got.err)
					}
					if cause := readControlAwait(t, closed, "broken partial stream close"); !errors.Is(cause, ErrOutboundOverflow) {
						t.Fatalf("close=%v, want outbound overflow cause", cause)
					}
					if wire := readPeerUntil(t, connection.peer, got.n, 10*time.Second); !bytes.Equal(wire, want[:got.n]) {
						t.Fatal("short write did not report the exact prefix already sent")
					}
				case "refused-overflow":
					if got.n != 0 || !errors.Is(got.err, ErrOutboundOverflow) || got.buffered != 0 || connection.conn.isClosing() {
						t.Fatalf("refused write=%+v closing=%v", got, connection.conn.isClosing())
					}
					if api == "WriteOwned" && !got.retained {
						t.Fatal("a pure overflow consumed its caller-owned buffer")
					}
				case "peer-closed":
					if got.n != 0 || got.err == nil || errors.Is(got.err, ErrOutboundOverflow) {
						t.Fatalf("broken peer write=%d,%v", got.n, got.err)
					}
					if cause := readControlAwait(t, closed, "failed socket write close"); !errors.Is(cause, got.err) {
						t.Fatalf("write error %v was lost by close %v", got.err, cause)
					}
				}
			})
		}
	}
}

func TestNativeQueuedCallbackWritesPropagateFlushFailure(t *testing.T) {
	for _, api := range []string{"Write", "Writev", "WriteOwned"} {
		t.Run(api, func(t *testing.T) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			replacement, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(replacement)
			results := make(chan error, 1)
			closed := make(chan error, 1)
			events := withFuncs(&Events{Pollers: 1, WriteBufferedThreshold: 32, MaxOutboundBuffered: 8}, FuncHandler{Open: func(c Conn) {
				if n, err := c.Write([]byte("head")); n != 4 || err != nil || c.OutboundBuffered() != 4 {
					t.Errorf("queued first write=%d,%v; pending=%d", n, err, c.OutboundBuffered())
				}
				if err := unix.Dup2(replacement, fds[1]); err != nil {
					t.Error(err)
				}
				var n int
				var err error
				switch api {
				case "Write":
					n, err = c.Write([]byte("new-data"))
				case "Writev":
					n, err = c.Writev([][]byte{[]byte("new-"), []byte("data")})
				case "WriteOwned":
					owned := AcquireBuffer(8)
					_, _ = owned.WriteString("new-data")
					n, err = c.WriteOwned(owned)
				}
				if n != 0 || err == nil || errors.Is(err, ErrOutboundOverflow) {
					t.Errorf("write after failed admission flush=%d,%v", n, err)
				}
				results <- err
			}, Close: func(_ Conn, err error) {
				closed <- err
			}})
			_, registered := startTestConnectionOn(t, events, fds)
			if err := <-registered; err != nil {
				t.Fatal(err)
			}
			cause := readControlAwait(t, results, "failed admission flush")
			if err := readControlAwait(t, closed, "failed flush closure"); !errors.Is(err, cause) {
				t.Fatalf("flush error %v lost as close cause %v", cause, err)
			}
		})
	}
}

func readPeerUntil(t *testing.T, fd int, size int, timeout time.Duration) []byte {
	t.Helper()
	result := make([]byte, size)
	offset := 0
	deadline := time.Now().Add(timeout)
	for offset < size {
		if time.Now().After(deadline) {
			t.Fatalf("read %d bytes, want %d", offset, size)
		}
		n, err := unix.Read(fd, result[offset:])
		if n > 0 {
			offset += n
		}
		if err == nil {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			time.Sleep(100 * time.Microsecond)
			continue
		}
		t.Fatal(err)
	}
	return result
}

func fillSocketUntilRefused(t *testing.T, fd int) int {
	t.Helper()
	total := 0
	chunk := make([]byte, 64<<10)
	for size := len(chunk); size > 0; size /= 2 {
		for {
			n, err := unix.Write(fd, chunk[:size])
			if n > 0 {
				total += n
			}
			if err != nil {
				if errors.Is(err, unix.EAGAIN) {
					break
				}
				t.Fatal(err)
			}
		}
	}
	return total
}

func drainPeerFully(fd int) int {
	buf := make([]byte, 1<<20)
	total := 0
	for {
		n, err := unix.Read(fd, buf)
		if n > 0 {
			total += n
		}
		if err != nil || n <= 0 {
			return total
		}
	}
}

func TestRefusedDirectSendKeepsAWritableEdge(t *testing.T) {
	payload := bytes.Repeat([]byte{'P'}, 100)
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 8<<10); err != nil {
		t.Fatal(err)
	}
	if err = unix.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	fillSocketUntilRefused(t, fds[0])
	var hooked atomic.Bool
	testHookDirectSendRefused = func(conn *fdConn) {
		if !hooked.CompareAndSwap(false, true) {
			return
		}
		drainPeerFully(fds[1])
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() {
		testHookDirectSendRefused = nil
	})
	events := &Events{Pollers: 1}
	funcs(events).Open = func(conn Conn) {
		if _, err := conn.Write(payload); err != nil {
			t.Error(err)
		}
	}
	testConn, registered := startTestConnectionOn(t, events, fds)
	if err = <-registered; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !hooked.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hooked.Load() {
		t.Fatal("the direct send was never refused")
	}
	got := 0
	buf := make([]byte, 4096)
	for got < len(payload) && time.Now().Before(deadline) {
		n, _ := unix.Read(testConn.peer, buf)
		if n > 0 {
			got += n
		}
		time.Sleep(time.Millisecond)
	}
	if got < len(payload) {
		c := testConn.conn
		t.Fatalf("stalled: peer got %d of %d bytes; pending=%d writeBlocked=%v", got, len(payload), c.pending.Load(), c.writeBlocked())
	}
}

type recoveringExecutor struct{ recovered atomic.Int64 }

func (e *recoveringExecutor) Submit(task IOTask) bool {
	go func() {
		defer func() {
			if recover() != nil {
				e.recovered.Add(1)
			}
		}()
		task.RunTask()
	}()
	return true
}

func (e *recoveringExecutor) SubmitBatch(tasks []IOTask) int {
	for _, task := range tasks {
		e.Submit(task)
	}
	return len(tasks)
}

func TestPanicInOnOutboundReleasesTheWriteClaim(t *testing.T) {
	for _, fromWriteTurn := range []bool{true, false} {
		name := "connection turn"
		if fromWriteTurn {
			name = "write turn"
		}
		t.Run(name, func(t *testing.T) {
			executor := &recoveringExecutor{}
			opened := make(chan *fdConn, 1)
			started := make(chan string, 1)
			var armed atomic.Bool
			events := &Events{Pollers: 1, Executor: executor}
			funcs(events).Start = func(ev *Events) {
				for _, listener := range ev.acceptor.listeners {
					started <- listener.pair.local.String()
					return
				}
			}
			funcs(events).Open = func(conn Conn) {
				opened <- conn.(*fdConn)
			}
			funcs(events).Read = func(conn Conn) error {
				_, _ = conn.Discard(-1)
				_, err := conn.Write([]byte("r"))
				return err
			}
			funcs(events).BytesWritten = func(Conn, int) {
				if armed.CompareAndSwap(true, false) {
					panic("OnWrite panics once")
				}
			}
			serveDone := make(chan error, 1)
			go func() {
				serveDone <- events.Serve("tcp://127.0.0.1:0")
			}()
			client, err := net.Dial("tcp", <-started)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server := <-opened
			for deadline := time.Now().Add(2 * time.Second); server.writeState.Load()&writeOpenedFlag == 0 || server.ioOwner.Load() != 0; {
				if time.Now().After(deadline) {
					t.Fatal("connection did not finish opening")
				}
				time.Sleep(time.Millisecond)
			}
			armed.Store(true)
			first := "x"
			if fromWriteTurn {
				_, err = server.Write([]byte(first))
			} else {
				first = "r"
				_, err = client.Write([]byte("q"))
			}
			if err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(2 * time.Second); executor.recovered.Load() == 0; {
				if time.Now().After(deadline) {
					t.Fatal("OnWrite did not panic")
				}
				time.Sleep(time.Millisecond)
			}
			if _, err = server.Write([]byte("later")); err != nil {
				t.Fatal(err)
			}
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			want := first + "later"
			got := make([]byte, len(want))
			if _, err = io.ReadFull(client, got); err != nil || string(got) != want {
				t.Fatalf("client read %q, %v; want %q", got, err, want)
			}
			_ = events.Close(nil)
			select {
			case <-serveDone:
			case <-time.After(3 * time.Second):
				t.Fatalf("Serve did not return after Close: writeState=%#x", server.writeState.Load())
			}
		})
	}
}

func TestPanicAfterBlockedFlushKeepsSending(t *testing.T) {
	for _, fromWriteTurn := range []bool{true, false} {
		name := "connection turn"
		if fromWriteTurn {
			name = "write turn"
		}
		t.Run(name, func(t *testing.T) {
			const total = 2 << 20
			payload := make([]byte, total)
			for i := range payload {
				payload[i] = byte(i * 11)
			}
			executor := &recoveringExecutor{}
			opened := make(chan *fdConn, 1)
			started := make(chan string, 1)
			var armed atomic.Bool
			var blockedAtPanic, ownerAtPanic atomic.Bool
			events := &Events{Pollers: 1, Executor: executor}
			funcs(events).Start = func(ev *Events) {
				for _, listener := range ev.acceptor.listeners {
					started <- listener.pair.local.String()
					return
				}
			}
			funcs(events).Open = func(conn Conn) {
				opened <- conn.(*fdConn)
			}
			funcs(events).Read = func(conn Conn) error {
				_, _ = conn.Discard(-1)
				_, err := conn.Write(payload)
				return err
			}
			funcs(events).BytesWritten = func(c Conn, _ int) {
				if armed.CompareAndSwap(true, false) {
					conn := c.(*fdConn)
					state := conn.writeState.Load()
					blockedAtPanic.Store(state&(writeBlockedFlag|writeEdgeFlag) != 0)
					ownerAtPanic.Store(conn.directOwner())
					panic("OnWrite panics once")
				}
			}
			serveDone := make(chan error, 1)
			go func() {
				serveDone <- events.Serve("tcp://127.0.0.1:0")
			}()
			t.Cleanup(func() {
				_ = events.Close(nil)
				<-serveDone
			})
			dialer := net.Dialer{Control: func(_, _ string, raw syscall.RawConn) error {
				var sockErr error
				if err := raw.Control(func(fd uintptr) {
					sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
				}); err != nil {
					return err
				}
				return sockErr
			}}
			client, err := dialer.Dial("tcp", <-started)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server := <-opened
			for deadline := time.Now().Add(2 * time.Second); server.writeState.Load()&writeOpenedFlag == 0 || server.ioOwner.Load() != 0; {
				if time.Now().After(deadline) {
					t.Fatal("connection did not finish opening")
				}
				time.Sleep(time.Millisecond)
			}
			if err := unix.SetsockoptInt(int(server.fd), unix.SOL_SOCKET, unix.SO_SNDBUF, 4096); err != nil {
				t.Fatal(err)
			}
			armed.Store(true)
			if fromWriteTurn {
				_, err = server.Write(payload)
			} else {
				_, err = client.Write([]byte("q"))
			}
			if err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(2 * time.Second); executor.recovered.Load() == 0; {
				if time.Now().After(deadline) {
					t.Fatal("OnWrite did not panic")
				}
				time.Sleep(time.Millisecond)
			}
			if !blockedAtPanic.Load() {
				t.Fatal("the flush that ran OnWrite was not refused by the socket")
			}
			if ownerAtPanic.Load() == fromWriteTurn {
				t.Fatalf("OnWrite ran on the wrong turn: connection turn = %v", ownerAtPanic.Load())
			}
			_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
			got := make([]byte, total)
			if n, err := io.ReadFull(client, got); err != nil {
				t.Fatalf("peer read %d of %d bytes: %v (pending=%d writeState=%#x)", n, total, err, server.pending.Load(), server.writeState.Load())
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("peer received a different stream")
			}
		})
	}
}

func TestOnOutboundWritesDoNotReenterIt(t *testing.T) {
	const total = 20000
	var depth, maxDepth, count atomic.Int32
	var mu sync.Mutex
	events := &Events{Pollers: 1}
	funcs(events).Read = func(c Conn) error {
		_, _ = c.Discard(-1)
		_, err := c.Write([]byte("a"))
		return err
	}
	funcs(events).BytesWritten = func(c Conn, n int) {
		mu.Lock()
		defer mu.Unlock()
		if d := depth.Add(1); d > maxDepth.Load() {
			maxDepth.Store(d)
		}
		defer depth.Add(-1)
		if count.Add(1) < total {
			_, _ = c.Write([]byte("b"))
			_ = c.Flush()
		}
	}
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("q")); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); count.Load() < total; {
		if time.Now().After(deadline) {
			t.Fatalf("OnWrite ran %d of %d times", count.Load(), total)
		}
		buf := make([]byte, 64<<10)
		if _, err := unix.Read(testConn.peer, buf); err != nil {
			runtime.Gosched()
		}
	}
	if d := maxDepth.Load(); d != 1 {
		t.Fatalf("OnWrite nested %d deep", d)
	}
}

func TestOutputAfterRecoveredOnOpenPanic(t *testing.T) {
	executor := &recoveringExecutor{}
	opened := make(chan Conn, 1)
	started := make(chan string, 1)
	events := &Events{Pollers: 1, Executor: executor}
	funcs(events).Start = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	funcs(events).Open = func(conn Conn) {
		written := make(chan error, 1)
		go func() {
			_, err := conn.Write([]byte("early"))
			written <- err
		}()
		if err := <-written; err != nil {
			t.Error(err)
		}
		opened <- conn
		panic("OnOpen panics")
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- events.Serve("tcp://127.0.0.1:0")
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	client, err := net.Dial("tcp", <-started)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-opened
	for deadline := time.Now().Add(2 * time.Second); executor.recovered.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("OnOpen did not panic")
		}
		time.Sleep(time.Millisecond)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 5)
	if _, err = io.ReadFull(client, buf); err != nil || string(buf) != "early" {
		t.Fatalf("client read %q, %v: output queued during OnOpen stayed queued", buf, err)
	}
	if _, err = server.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadFull(client, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("client read %q, %v after a recovered OnOpen panic", buf, err)
	}
}

func TestKickAfterShutdownBeganReleasesTheClaim(t *testing.T) {
	events := &Events{Pollers: 1}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		loop.ioPool.stop()
		_ = loop.poller.Close(nil)
	}()
	conn := &fdConn{fd: -1}
	conn.loop = loop
	conn.entry = events.plainEntry()
	conn.writeState.Store(writeOpenedFlag)
	conn.pending.Store(1)
	loop.stopIO()
	conn.kickWriter()
	if conn.writeClaimed() {
		t.Fatal("a kick after shutdown began kept the write claim")
	}
}

type writeTurnRejectingExecutor struct{ refused atomic.Int64 }

func (e *writeTurnRejectingExecutor) Submit(task IOTask) bool {
	if _, ok := task.(*writeTurn); ok {
		e.refused.Add(1)
		return false
	}
	go task.RunTask()
	return true
}

func (e *writeTurnRejectingExecutor) SubmitBatch(tasks []IOTask) int {
	for index, task := range tasks {
		if !e.Submit(task) {
			return index
		}
	}
	return len(tasks)
}

func TestRefusedWriteTurnClosesTheConnection(t *testing.T) {
	executor := &writeTurnRejectingExecutor{}
	opened := make(chan *fdConn, 1)
	closed := make(chan error, 1)
	started := make(chan string, 1)
	events := &Events{Pollers: 1, Executor: executor}
	funcs(events).Start = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	funcs(events).Open = func(conn Conn) {
		opened <- conn.(*fdConn)
	}
	funcs(events).Close = func(_ Conn, err error) {
		closed <- err
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- events.Serve("tcp://127.0.0.1:0")
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	client, err := net.Dial("tcp", <-started)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-opened
	for deadline := time.Now().Add(2 * time.Second); server.writeState.Load()&writeOpenedFlag == 0 || server.ioOwner.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("connection did not finish opening")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = server.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-closed:
		if !errors.Is(err, net.ErrClosed) || executor.refused.Load() == 0 {
			t.Fatalf("OnClose(%v) after %d refused write turns", err, executor.refused.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the refused write turn did not close the connection")
	}
}

func TestCloseNeverWritesReusedDescriptor(t *testing.T) {
	opened := make(chan *fdConn, 4)
	closed := make(chan *fdConn, 4)
	started := make(chan string, 1)
	var strayInbound atomic.Int64
	events := &Events{Pollers: 1}
	funcs(events).Start = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	funcs(events).Open = func(conn Conn) {
		opened <- conn.(*fdConn)
	}
	funcs(events).Read = func(conn Conn) error {
		n, _ := conn.Discard(-1)
		strayInbound.Add(int64(n))
		return nil
	}
	funcs(events).Close = func(conn Conn, _ error) {
		closed <- conn.(*fdConn)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- events.Serve("tcp://127.0.0.1:0")
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	var addr string
	select {
	case addr = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not start")
	}
	reused := 0
	for iteration := 0; iteration < 30; iteration++ {
		clientA, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		serverA := <-opened
		fdA := serverA.fd
		go func() {
			_, _ = io.Copy(io.Discard, clientA)
		}()
		var producers sync.WaitGroup
		chunk := bytes.Repeat([]byte{'A'}, 1024)
		for range 4 {
			producers.Add(1)
			go func() {
				defer producers.Done()
				for {
					if _, err := serverA.Write(chunk); err != nil {
						return
					}
				}
			}()
		}
		time.Sleep(2 * time.Millisecond)
		_ = serverA.Abort(io.EOF)
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("OnClose was not delivered")
		}
		clientB, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		serverB := <-opened
		if serverB.fd == fdA {
			reused++
		}
		_ = clientB.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		var probe [1]byte
		if n, _ := clientB.Read(probe[:]); n > 0 {
			t.Fatalf("connection B received %q written for A on descriptor %d", probe[:n], fdA)
		}
		producers.Wait()
		_ = clientA.Close()
		_ = serverB.Abort(io.EOF)
		<-closed
		_ = clientB.Close()
		if n := strayInbound.Load(); n != 0 {
			t.Fatalf("server received %d bytes no client sent", n)
		}
	}
	t.Logf("descriptor reused by the accepted end in %d of 30 iterations", reused)
}

func TestCoalesceOwnedCopiesOnlySmallOrSparseBuffers(t *testing.T) {
	const block = 32 << 10
	cases := []struct {
		name      string
		size, cap int
		want      bool
	}{{"small", 512, 16 << 10, true}, {"at the small limit", coalesceOwnedMax, coalesceOwnedMax, true}, {"large and full", 16120, 16 << 10, false}, {"large and half empty", 6 << 10, 16 << 10, true}, {"large and half full", 8 << 10, 16 << 10, false}, {"past half a block", block/2 + 1, 64 << 10, false}}
	for _, c := range cases {
		owned := bytebuf.AcquireBuffer(c.cap)
		owned.CommitWrite(c.size)
		if got := coalesceOwned(owned, c.size, block); got != c.want {
			t.Errorf("%s: coalesceOwned(%d of %d) = %v, want %v", c.name, c.size, owned.Cap(), got, c.want)
		}
		bytebuf.ReleaseBuffer(owned)
	}
}

func runBidirectionalLimitTransfer(t *testing.T, raw [2]net.Conn, limit int) {
	t.Helper()
	total := int64(limit) * 7 / 8
	releaseOpen := make(chan struct{})
	var received [2]atomic.Int64
	var conns [2]Conn
	var events [2]*Events
	var done [2]chan error
	for i := range 2 {
		started := make(chan struct{})
		opened := make(chan Conn, 1)
		ev := &Events{Pollers: 1, MaxOutboundBuffered: limit, ReadBufferSize: 16 << 10}
		funcs(ev).Start = func(*Events) {
			close(started)
		}
		funcs(ev).Open = func(conn Conn) {
			opened <- conn
			<-releaseOpen
		}
		funcs(ev).Read = func(conn Conn) error {
			n, _ := conn.Discard(conn.InboundBuffered())
			received[i].Add(int64(n))
			return nil
		}
		events[i] = ev
		done[i] = make(chan error, 1)
		go func() {
			done[i] <- ev.Serve()
		}()
		<-started
		conn, err := ev.Adopt(raw[i])
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
			t.Fatalf("both directions stopped: received %d/%d and %d/%d; pending %d,%d; writeBlocked %v,%v", a, total, b, total, first.pending.Load(), second.pending.Load(), first.writeBlocked(), second.writeBlocked())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bidirectional transfer did not complete")
}

func TestBidirectionalOutboundLimitTCP(t *testing.T) {
	first, second := tcpConnectionPair(t)
	defer first.Close()
	defer second.Close()
	for _, conn := range []net.Conn{first, second} {
		_ = conn.(*net.TCPConn).SetReadBuffer(pinnedSocketBuffer)
		_ = conn.(*net.TCPConn).SetWriteBuffer(pinnedSocketBuffer)
	}
	runBidirectionalLimitTransfer(t, [2]net.Conn{first, second}, 2<<20)
}

func TestCloseWriteWaitsForAFullSocketWithoutSpinning(t *testing.T) {
	executor := &countingExecutor{}
	want := make([]byte, 8<<20)
	for i := range want {
		want[i] = byte(i * 13)
	}
	queued := make(chan error, 1)
	events := withFuncs(&Events{Pollers: 1, Executor: executor}, FuncHandler{Open: func(c Conn) {
		_, err := c.Write(want)
		if err == nil {
			err = c.CloseWrite()
		}
		queued <- err
	}})
	client, _ := serveInboundTest(t, events)
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	before := executor.turns.Load()
	time.Sleep(100 * time.Millisecond)
	if spun := executor.turns.Load() - before; spun > 10 {
		t.Fatalf("%d turns scheduled in 100ms while CloseWrite waited for a full socket", spun)
	}
	if got := readAllWithin(t, client, 20*time.Second); !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes before the end of stream, want %d intact", len(got), len(want))
	}
}

func TestExternalUDPServerPeerWrites(t *testing.T) {
	started := make(chan string, 1)
	peer := make(chan Conn, 1)
	closed := make(chan struct{}, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: 8}
	funcs(events).Start = func(ev *Events) {
		ev.acceptor.mux.Lock()
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			break
		}
		ev.acceptor.mux.Unlock()
	}
	funcs(events).Read = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		select {
		case peer <- conn:
		default:
		}
		return nil
	}
	funcs(events).Close = func(Conn, error) {
		closed <- struct{}{}
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- events.Serve("udp://127.0.0.1:0")
	}()
	defer func() {
		_ = events.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Error("UDP server did not stop")
		}
	}()
	var address string
	select {
	case address = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("UDP server did not start")
	}
	remote, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Write([]byte("open")); err != nil {
		t.Fatal(err)
	}
	var conn Conn
	select {
	case conn = <-peer:
	case <-time.After(3 * time.Second):
		t.Fatal("UDP peer callback did not run")
	}
	for _, payload := range []string{"plain", "owned"} {
		var n int
		if payload == "plain" {
			n, err = conn.Write([]byte(payload))
		} else {
			buffer := AcquireBuffer(len(payload))
			_, _ = buffer.WriteString(payload)
			n, err = conn.WriteOwned(buffer)
		}
		if n != len(payload) || err != nil {
			t.Fatalf("external UDP write %q = %d, %v", payload, n, err)
		}
		if err = client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var received [16]byte
		read, readErr := client.Read(received[:])
		if readErr != nil || string(received[:read]) != payload {
			t.Fatalf("UDP datagram = %q, %v; want %q", received[:read], readErr, payload)
		}
	}
	if n, err := conn.Write([]byte("too-large")); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("UDP overflow = %d, %v", n, err)
	}
	if n, err := conn.Writev([][]byte{[]byte("ab"), []byte("cd")}); n != 4 || err != nil {
		t.Fatalf("UDP Writev = %d, %v", n, err)
	}
	for _, want := range []string{"ab", "cd"} {
		if err = client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var received [16]byte
		read, readErr := client.Read(received[:])
		if readErr != nil || string(received[:read]) != want {
			t.Fatalf("UDP Writev datagram = %q, %v; want %q", received[:read], readErr, want)
		}
	}
	if got := conn.OutboundBuffered(); got != 0 {
		t.Fatalf("pending UDP bytes = %d", got)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("UDP peer close callback did not run")
	}
	if n, err := conn.Write([]byte("late")); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after close = %d, %v", n, err)
	}
}

func TestUDPChildWriteAfterServerRelease(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	events := &Events{ReadBufferSize: 64}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = loop.poller.Close(nil)
		loop.ioPool.stop()
	}()
	server := &fdConn{fd: int32(fds[0]), udp: &unixUDPState{peers: map[socket.UDPAddress]*fdConn{}}}
	server.loop, server.internal = loop, true
	server.entry = events.plainEntry()
	child := &fdConn{fd: int32(fds[0]), udp: &unixUDPState{server: server}}
	child.loop = loop
	child.entry = events.plainEntry()
	if n, err := child.Write([]byte("x")); n != 1 || err != nil {
		t.Fatalf("child write = %d, %v", n, err)
	}
	if got := string(readPeer(t, fds[1], 1)); got != "x" {
		t.Fatalf("datagram = %q", got)
	}
	if _, ok := server.teardown(nil); !ok {
		t.Fatal("server was not released")
	}
	if n, err := child.Write([]byte("y")); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after the server's release = %d, %v", n, err)
	}
}

func TestUnixTurnScratchReserveSendHandOverAndDrop(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	ev := &Events{WriteBufferedThreshold: 64, MaxOutboundBuffered: 512}
	_ = ev.initConfig()
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.closePoller(nil)

	conn := &fdConn{fd: int32(fds[0]), turn: turnCorked}
	conn.entry = ev.plainEntry()
	conn.loop = loop

	// Off-turn ReserveTurnWrite returns false.
	if _, ok, _ := conn.ReserveTurnWrite(4); ok {
		t.Fatal("off-turn ReserveTurnWrite succeeded")
	}

	conn.ioOwner.Store(currentGoroutineID())
	holder := ev.readPool.Get()
	conn.scratch = holder

	// ReserveTurnWrite + Writev + sendScratch on live socketpair!
	dst, ok, err := conn.ReserveTurnWrite(4)
	if !ok || err != nil {
		t.Fatalf("ReserveTurnWrite = %v, %v", ok, err)
	}
	copy(dst, "ab12")
	if n, err := conn.Writev([][]byte{[]byte("cd"), []byte("34")}); err != nil || n != 4 {
		t.Fatalf("Writev scratch = %d, %v", n, err)
	}
	conn.scratch = nil
	if err := conn.sendScratch(holder); err != nil {
		t.Fatalf("sendScratch = %v", err)
	}
	conn.releaseWrite()
	if got := string(readPeer(t, fds[1], 8)); got != "ab12cd34" {
		t.Fatalf("peer read = %q, want ab12cd34", got)
	}

	// Overflow, handOverScratch, blocked sendScratch, and dropScratch!
	conn.scratch = holder
	if _, ok, err := conn.ReserveTurnWrite(1024); !ok || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("overflow ReserveTurnWrite = %v, %v", ok, err)
	}
	dst2, ok, err := conn.ReserveTurnWrite(3)
	if !ok || err != nil {
		t.Fatal(err)
	}
	copy(dst2, "xyz")
	conn.setWriteBlocked(true)
	conn.scratch = nil
	_ = conn.sendScratch(holder)
	conn.setWriteBlocked(false)
	conn.submitMu.Lock()
	conn.outbound.Reset()
	conn.submitMu.Unlock()
	conn.pending.Store(0)
	conn.releaseWrite()

	holderDrop := ev.readPool.Get()
	conn.scratch = holderDrop
	_, _, _ = conn.ReserveTurnWrite(5)
	conn.dropScratch()
	conn.releaseWrite()

	// Exercise scratch spill -> handOverScratch + outbound.Reserve fallback!
	evBig := &Events{WriteBufferedThreshold: 64, MaxOutboundBuffered: 1 << 20}
	_ = evBig.initConfig()
	conn.entry = evBig.plainEntry()
	holderSpill := evBig.readPool.Get()
	conn.scratch = holderSpill
	if _, ok, err := conn.ReserveTurnWrite(4096); !ok || err != nil {
		t.Fatalf("initial 4096 reserve = %v, %v", ok, err)
	}
	if _, ok, err := conn.ReserveTurnWrite(32 << 10); !ok || err != nil {
		t.Fatalf("spill 32K reserve = %v, %v", ok, err)
	}
	conn.scratch = nil
	conn.submitMu.Lock()
	conn.outbound.Reset()
	conn.submitMu.Unlock()
	conn.pending.Store(0)
	conn.releaseWrite()

	// Exercise sendScratch hitting EAGAIN / partial send and fatal fd error!
	_ = unix.SetNonblock(fds[0], true)
	fill := make([]byte, 32<<10)
	for {
		_, werr := unix.Write(fds[0], fill)
		if werr != nil {
			break
		}
	}
	holderFull := evBig.readPool.Get()
	conn.scratch = holderFull
	_, _, _ = conn.ReserveTurnWrite(64)
	conn.scratch = nil
	_ = conn.sendScratch(holderFull) // hits EAGAIN + queueDirectSuffix!
	conn.submitMu.Lock()
	conn.outbound.Reset()
	conn.submitMu.Unlock()
	conn.pending.Store(0)
	conn.releaseWrite()

	badConn := &fdConn{fd: -1, turn: turnCorked}
	badConn.entry = evBig.plainEntry()
	badConn.loop = loop
	badConn.ioOwner.Store(currentGoroutineID())
	holderBad := evBig.readPool.Get()
	badConn.scratch = holderBad
	_, _, _ = badConn.ReserveTurnWrite(8)
	badConn.scratch = nil
	if err := badConn.sendScratch(holderBad); err == nil {
		t.Fatal("expected EBADF from sendScratch on fd -1")
	}
	badConn.submitMu.Lock()
	badConn.outbound.Reset()
	badConn.submitMu.Unlock()
	badConn.pending.Store(0)
	badConn.releaseWrite()
	badConn.ioOwner.Store(0)

	// Exercise writeQueuedOnLoop / writevQueuedOnLoop fallback when scratch is nil.
	conn.scratch = nil
	_, _ = conn.Write([]byte("fallback1"))
	_, _ = conn.Writev([][]byte{[]byte("fb"), []byte("2")})
	conn.submitMu.Lock()
	conn.outbound.Reset()
	conn.submitMu.Unlock()
	conn.pending.Store(0)

	// Contended writeOwnerFlag returns (nil, false, nil), and outputClosed returns (nil, true, net.ErrClosed)!
	conn.scratch = holderFull
	conn.writeState.Or(writeOwnerFlag)
	if _, ok, err := conn.ReserveTurnWrite(8); ok || err != nil {
		t.Fatalf("contended ReserveTurnWrite = %v, %v, want false, nil", ok, err)
	}
	conn.writeState.And(^writeOwnerFlag)
	conn.writeState.Or(writeShutFlag)
	if _, ok, err := conn.ReserveTurnWrite(8); !ok || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed ReserveTurnWrite = %v, %v, want true, net.ErrClosed", ok, err)
	}
	conn.scratch = nil
	conn.ioOwner.Store(0)
}
