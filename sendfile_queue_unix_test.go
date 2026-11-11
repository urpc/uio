//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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

package uio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, name string, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*31 + i>>9)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func socketOfConn(c Conn) *fdConn {
	switch x := c.(type) {
	case *fdConn:
		return x
	case *layerNode:
		fdc, _ := x.stack.socket.(*fdConn)
		return fdc
	}
	return nil
}

func queuedFileRanges(c *fdConn) int {
	c.submitMu.Lock()
	defer c.submitMu.Unlock()
	if zc := c.loadZeroCopy(); zc != nil {
		return zc.files.len()
	}
	return 0
}

// servePair serves one connection whose socket buffers are sockBuf bytes, or
// the system's when sockBuf is 0, and runs server with it on a goroutine of
// its own and client with the dialed end; it returns what client returns.
// ev.Handler, if set, hears the connection's OnClose.
func servePair(t *testing.T, ev *Events, sockBuf int, server func(c Conn), client func(cli net.Conn) []byte) []byte {
	t.Helper()
	return servePairOpts(t, ev, nil, sockBuf, server, client)
}

func servePairOpts(t *testing.T, ev *Events, opts []ListenOption, sockBuf int, server func(c Conn), client func(cli net.Conn) []byte) []byte {
	t.Helper()
	orig := ev.Handler
	ev.Handler = FuncHandler{
		Open: func(c Conn) {
			if fdc := socketOfConn(c); fdc != nil && sockBuf > 0 {
				_ = syscall.SetsockoptInt(int(fdc.fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, sockBuf)
			}
			if orig != nil {
				orig.OnOpen(c)
			}
			go server(c)
		},
		Read: func(c Conn) error {
			if orig != nil {
				return orig.OnRead(c)
			}
			return nil
		},
		Timer: func(c Conn) error {
			if orig != nil {
				return orig.OnTimer(c)
			}
			return nil
		},
		Close: func(c Conn, err error) {
			if orig != nil {
				orig.OnClose(c, err)
			}
		},
	}
	stop := startBlockingEvents(t, ev)
	defer stop()
	ln, err := ev.Listen("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatal(err)
	}
	dialer := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		return rc.Control(func(fd uintptr) {
			if sockBuf > 0 {
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, sockBuf)
			}
		})
	}}
	cli, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	_ = cli.SetReadDeadline(time.Now().Add(20 * time.Second))
	return client(cli)
}

// closeHandler is a Handler that only hears OnClose.
type closeHandler func(error)

func (h closeHandler) OnOpen(Conn)               {}
func (h closeHandler) OnRead(Conn) error         { return nil }
func (h closeHandler) OnTimer(Conn) error        { return nil }
func (h closeHandler) OnClose(_ Conn, err error) { h(err) }

// sendFilePair serves one connection whose socket buffers are small, runs
// server with it off the connection's turn, and returns what the client read
// once server returned and closed the connection. Unless concurrent is set,
// the client reads nothing until server returns, so output beyond the buffers
// queues. onClose, if set, hears the connection's OnClose.
func sendFilePair(t *testing.T, ev *Events, opts []ListenOption, concurrent bool, onClose func(error), server func(c Conn)) []byte {
	t.Helper()
	if onClose != nil {
		ev.Handler = closeHandler(onClose)
	} else {
		ev.Handler = nil
	}
	done := make(chan struct{})
	return servePairOpts(t, ev, opts, 16<<10, func(c Conn) {
		defer close(done)
		server(c)
	}, func(cli net.Conn) []byte {
		read := make(chan []byte, 1)
		readAll := func() {
			b, err := io.ReadAll(cli)
			if err != nil {
				t.Errorf("client read: %v", err)
			}
			read <- b
		}
		if concurrent {
			go readAll()
		}
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("server side did not finish")
		}
		if !concurrent {
			go readAll()
		}
		return <-read
	})
}

// The rest of a range a direct sendfile(2) began goes ahead of what another
// write queued while that send held the claim.
func TestSendFileRestPrecedesWritesQueuedDuringItsSend(t *testing.T) {
	path, data := writeTestFile(t, "fifo", 1<<20)
	after := []byte("WRITTEN-DURING-THE-SEND")
	got := sendFilePair(t, &Events{}, nil, false, nil, func(c Conn) {
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		fired := false
		hook := func(*fdConn) {
			if !fired {
				fired = true
				if _, err := c.Write(after); err != nil {
					t.Errorf("write during the send: %v", err)
				}
			}
		}
		testHookSendFileDirect.Store(&hook)
		defer testHookSendFileDirect.Store(nil)
		if n, err := SendFile(c, f, 0, -1); n != int64(len(data)) || err != nil {
			t.Errorf("SendFile = (%d, %v)", n, err)
		}
		if !fired {
			t.Error("the direct send took the whole file; the socket buffers are too large for this test")
		}
		_ = c.Close()
	})
	want := append(append([]byte(nil), data...), after...)
	if !bytes.Equal(got, want) {
		t.Fatalf("stream of %d bytes differs from file+write (%d bytes); write found at %d",
			len(got), len(want), bytes.Index(got, after))
	}
}

// A range that continues the last queued one of the same file extends it, so
// a file sent in pieces holds one descriptor; a short range is queued as
// bytes and holds none; writes between ranges keep them apart.
func TestSendFileQueuedRangesShareADescriptor(t *testing.T) {
	path, data := writeTestFile(t, "pieces", 2<<20)
	filler := bytes.Repeat([]byte{'F'}, 1<<20)
	mid := []byte("MID")
	got := sendFilePair(t, &Events{}, nil, false, nil, func(c Conn) {
		fdc := socketOfConn(c)
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		if _, err := c.Write(filler); err != nil {
			t.Error(err)
			return
		}
		send := func(off, n int64) {
			if got, err := SendFile(c, f, off, n); got != n || err != nil {
				t.Errorf("SendFile(%d, %d) = (%d, %v)", off, n, got, err)
			}
		}
		send(0, 512<<10)
		send(512<<10, 512<<10)
		if n := queuedFileRanges(fdc); n != 1 {
			t.Errorf("two contiguous ranges hold %d descriptors, want 1", n)
		}
		send(1<<20, 16<<10)
		if n := queuedFileRanges(fdc); n != 1 {
			t.Errorf("a 16 KiB range was queued with a descriptor: %d ranges", n)
		}
		_, _ = c.Write(mid)
		send(1<<20+16<<10, 1<<20-16<<10)
		if n := queuedFileRanges(fdc); n != 2 {
			t.Errorf("a range after a write merged across it: %d ranges, want 2", n)
		}
		_ = c.Close()
	})
	want := append(append(append(append([]byte(nil), filler...), data[:1<<20+16<<10]...), mid...), data[1<<20+16<<10:]...)
	if !bytes.Equal(got, want) {
		t.Fatalf("stream differs: got %d bytes, want %d", len(got), len(want))
	}
}

// A queued range holds no memory: it is admitted whole under a limit far
// shorter than itself, and the writes that follow wait for it to drain.
func TestSendFileRangeIsAdmittedWholeUnderTheLimit(t *testing.T) {
	path, data := writeTestFile(t, "admitted", 4<<20)
	got := sendFilePair(t, &Events{MaxOutboundBuffered: 64 << 10}, nil, false, nil, func(c Conn) {
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		if n, err := SendFile(c, f, 0, -1); n != int64(len(data)) || err != nil {
			t.Errorf("SendFile of 4 MiB under a 64 KiB limit = (%d, %v)", n, err)
		}
		if c.OutboundBuffered() <= 64<<10 {
			t.Errorf("OutboundBuffered = %d, want the queued range counted", c.OutboundBuffered())
		}
		if _, err := c.Write([]byte("x")); !errors.Is(err, ErrOutboundOverflow) {
			t.Errorf("write behind the range = %v, want ErrOutboundOverflow", err)
		}
		if _, err := SendFile(c, f, 0, 1); !errors.Is(err, ErrOutboundOverflow) {
			t.Errorf("SendFile into a full queue = %v, want ErrOutboundOverflow", err)
		}
		_ = c.Close()
	})
	if !bytes.Equal(got, data) {
		t.Fatalf("client read %d bytes, want the %d-byte file", len(got), len(data))
	}
}

// A sendfile(2) that fails on a broken socket reports the error and closes
// the connection rather than queueing the rest.
func TestSendFileReportsAFailedSocket(t *testing.T) {
	path, _ := writeTestFile(t, "broken", 64<<10)
	closed := make(chan error, 1)
	sendFilePair(t, &Events{}, nil, false, func(err error) { closed <- err }, func(c Conn) {
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		_ = syscall.Shutdown(int(socketOfConn(c).fd), syscall.SHUT_WR)
		if n, err := SendFile(c, f, 0, -1); err == nil || n != 0 {
			t.Errorf("SendFile on a shut socket = (%d, %v), want an error", n, err)
		}
	})
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stayed open after its send failed")
	}
}

// Without sendfile(2) SendFile reads the range into memory: a non-blocking
// call takes no more than the budget, while a BlockingWriter streams any size.
func TestSendFileUserspaceRangeIsBounded(t *testing.T) {
	path, data := writeTestFile(t, "layered", sendfileReadMax+1<<20)
	t.Run("non-blocking", func(t *testing.T) {
		got := sendFilePair(t, &Events{}, []ListenOption{WithLayers(PassThroughLayer)}, false, nil, func(c Conn) {
			f, err := os.Open(path)
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			if n, err := SendFile(c, f, 0, -1); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
				t.Errorf("SendFile past the budget = (%d, %v), want (0, ErrOutboundOverflow)", n, err)
			}
			if n, err := SendFile(c, f, 0, 1<<20); n != 1<<20 || err != nil {
				t.Errorf("SendFile within the budget = (%d, %v)", n, err)
			}
			_ = c.Close()
		})
		if !bytes.Equal(got, data[:1<<20]) {
			t.Fatalf("client read %d bytes, want the first MiB of the file", len(got))
		}
	})
	t.Run("blocking", func(t *testing.T) {
		got := sendFilePair(t, &Events{BlockingWrite: true}, []ListenOption{WithLayers(PassThroughLayer)}, true, nil, func(c Conn) {
			f, err := os.Open(path)
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			if n, err := SendFile(c, f, 0, -1); n != int64(len(data)) || err != nil {
				t.Errorf("blocking SendFile = (%d, %v)", n, err)
			}
			_ = c.Close()
		})
		if !bytes.Equal(got, data) {
			t.Fatalf("client read %d bytes, want %d", len(got), len(data))
		}
	})
}

// A cancelled SendFileContext drops only the ranges it queued: a range
// another caller queued before it still reaches the peer whole.
func TestSendFileContextCancelKeepsOtherCallersRanges(t *testing.T) {
	pathA, dataA := writeTestFile(t, "a", 1<<20)
	pathB, _ := writeTestFile(t, "b", 4<<20)
	filler := bytes.Repeat([]byte{'F'}, 1<<20)
	got := sendFilePair(t, &Events{}, nil, false, nil, func(c Conn) {
		fa, err := os.Open(pathA)
		if err != nil {
			t.Error(err)
			return
		}
		defer fa.Close()
		fb, err := os.Open(pathB)
		if err != nil {
			t.Error(err)
			return
		}
		defer fb.Close()
		if _, err := c.Write(filler); err != nil {
			t.Error(err)
			return
		}
		if n, err := SendFile(c, fa, 0, -1); n != int64(len(dataA)) || err != nil {
			t.Errorf("SendFile(a) = (%d, %v)", n, err)
		}
		bw, err := NewBlockingWriter(c, WithBufferSize(4<<20))
		if err != nil {
			t.Error(err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		n, err := bw.SendFileContext(ctx, fb, 0, -1)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("SendFileContext(b) = (%d, %v), want DeadlineExceeded", n, err)
		}
		_ = c.Close()
	})
	want := append(append([]byte(nil), filler...), dataA...)
	if len(got) < len(want) || !bytes.Equal(got[:len(want)], want) {
		t.Fatalf("the stream lost the other caller's range: read %d bytes, want %d first", len(got), len(want))
	}
}

// A file truncated while its range waits in the queue fails the stream: the
// peer gets the part the file still held and nothing written after the range,
// which it would take for the range's own bytes.
func TestSendFileTruncatedInTheQueueFailsTheStream(t *testing.T) {
	path, data := writeTestFile(t, "trunc", 2<<20)
	filler := bytes.Repeat([]byte{'F'}, 1<<20)
	next := []byte("<<THE NEXT RESPONSE>>")
	closed := make(chan error, 1)
	got := sendFilePair(t, &Events{}, nil, false, func(err error) { closed <- err }, func(c Conn) {
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		if _, err := c.Write(filler); err != nil {
			t.Error(err)
			return
		}
		if n, err := SendFile(c, f, 0, int64(len(data))); n != int64(len(data)) || err != nil {
			t.Errorf("SendFile = (%d, %v)", n, err)
		}
		if _, err := c.Write(next); err != nil {
			t.Error(err)
		}
		if err := os.Truncate(path, 256<<10); err != nil {
			t.Error(err)
		}
		_ = c.Close()
	})
	want := append(append([]byte(nil), filler...), data[:256<<10]...)
	if !bytes.Equal(got, want) {
		t.Fatalf("the peer got %d bytes, want the %d before the file ended; the next response is at %d",
			len(got), len(want), bytes.Index(got, next))
	}
	if err := <-closed; !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("OnClose = %v, want io.ErrUnexpectedEOF", err)
	}
}

// A SendFileContext cut short reports what reached the peer, and that is a
// prefix of the file: the rest of the range is withdrawn whole, so a caller
// resuming at the count neither skips nor repeats a byte.
func TestSendFileContextCutShortSendsAPrefix(t *testing.T) {
	path, data := writeTestFile(t, "prefix", 8<<20)
	const window = 1 << 20
	full := make(chan struct{})
	cancelNow := make(chan struct{})
	count := make(chan int64, 1)
	got := servePair(t, &Events{}, 16<<10, func(c Conn) {
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		bw, err := NewBlockingWriter(c, WithBufferSize(window))
		if err != nil {
			t.Error(err)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for c.OutboundBuffered() < window {
				time.Sleep(time.Millisecond)
			}
			close(full)
			<-cancelNow
			cancel()
		}()
		n, err := bw.SendFileContext(ctx, f, 0, -1)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("SendFileContext = (%d, %v), want context.Canceled", n, err)
		}
		count <- n
		_ = c.Close()
	}, func(cli net.Conn) []byte {
		<-full
		buf := make([]byte, 24<<10)
		k, _ := io.ReadFull(cli, buf)
		time.Sleep(50 * time.Millisecond)
		close(cancelNow)
		rest, _ := io.ReadAll(cli)
		return append(buf[:k], rest...)
	})
	n := <-count
	if int64(len(got)) != n || !bytes.Equal(got, data[:len(got)]) {
		t.Fatalf("SendFileContext reported %d bytes; the peer got %d, a prefix of the file: %v",
			n, len(got), bytes.Equal(got, data[:min(len(got), len(data))]))
	}
}

// A SendFileContext cut short while a write turn's sendfile(2) reads its range
// counts the part that call reads: it reaches the peer.
func TestSendFileContextCutDuringASendCountsThePartSent(t *testing.T) {
	path, data := writeTestFile(t, "sending", 16<<20)
	const window = 4 << 20
	full := make(chan struct{})
	sending := make(chan struct{})
	release := make(chan struct{})
	count := make(chan int64, 1)
	var target atomic.Pointer[fdConn]
	var once atomic.Bool
	hook := func(c *fdConn) {
		if c == target.Load() && once.CompareAndSwap(false, true) {
			close(sending)
			<-release
		}
	}
	testHookQueuedSendfile.Store(&hook)
	defer testHookQueuedSendfile.Store(nil)
	got := servePair(t, &Events{}, 16<<10, func(c Conn) {
		target.Store(socketOfConn(c))
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		bw, err := NewBlockingWriter(c, WithBufferSize(window))
		if err != nil {
			t.Error(err)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for c.OutboundBuffered() < window {
				time.Sleep(time.Millisecond)
			}
			close(full)
			<-sending
			cancel()
		}()
		n, _ := bw.SendFileContext(ctx, f, 0, -1)
		count <- n
		close(release)
		_ = c.Close()
	}, func(cli net.Conn) []byte {
		<-full
		b, _ := io.ReadAll(cli)
		return b
	})
	n := <-count
	if int64(len(got)) != n || !bytes.Equal(got, data[:len(got)]) {
		t.Fatalf("SendFileContext reported %d bytes; the peer got %d", n, len(got))
	}
}

// A SendFile the queue refuses after a CloseWrite came in between its
// admission and its queueing leaves the CloseWrite to finish: the peer sees
// the end of the stream.
func TestSendFileRefusedAfterCloseWriteLeavesTheEndOfStream(t *testing.T) {
	path, _ := writeTestFile(t, "refused", 1<<20)
	inHook := make(chan struct{})
	resume := make(chan struct{})
	var once atomic.Bool
	hook := func(*fdConn) {
		if once.CompareAndSwap(false, true) {
			close(inHook)
			<-resume
		}
	}
	testHookQueueRangeDup.Store(&hook)
	defer testHookQueueRangeDup.Store(nil)
	sendErr := make(chan error, 1)
	ev := &Events{Handler: FuncHandler{
		Open: func(c Conn) {
			go func() {
				f, err := os.Open(path)
				if err != nil {
					sendErr <- err
					return
				}
				defer f.Close()
				_, err = SendFile(c, f, 0, 1<<20)
				sendErr <- err
			}()
			// OnOpen has not returned, so that SendFile queues its range.
			<-inHook
			if err := c.CloseWrite(); err != nil {
				t.Errorf("CloseWrite: %v", err)
			}
			go func() {
				time.Sleep(50 * time.Millisecond)
				close(resume)
			}()
		},
	}}
	stop := startBlockingEvents(t, ev)
	defer stop()
	ln, err := ev.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := <-sendErr; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("SendFile after CloseWrite = %v, want net.ErrClosed", err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(cli); err != nil || len(b) != 0 {
		t.Fatalf("the peer read %d bytes and %v, want the end of the stream", len(b), err)
	}
}

// A SendFileContext waiting for its range to drain returns once the peer
// resets the connection, with the error, instead of waiting for a drain that
// never comes.
func TestSendFileContextReturnsWhenThePeerResets(t *testing.T) {
	path, _ := writeTestFile(t, "reset", 64<<20)
	result := make(chan struct {
		n   int64
		err error
	}, 1)
	servePair(t, &Events{}, 16<<10, func(c Conn) {
		f, err := os.Open(path)
		if err != nil {
			result <- struct {
				n   int64
				err error
			}{0, err}
			return
		}
		defer f.Close()
		bw, err := NewBlockingWriter(c, WithBufferSize(256<<10))
		if err != nil {
			result <- struct {
				n   int64
				err error
			}{0, err}
			return
		}
		n, err := bw.SendFileContext(context.Background(), f, 0, -1)
		result <- struct {
			n   int64
			err error
		}{n, err}
	}, func(cli net.Conn) []byte {
		buf := make([]byte, 1<<20)
		_, _ = io.ReadFull(cli, buf)
		_ = cli.(*net.TCPConn).SetLinger(0)
		_ = cli.Close()
		return nil
	})
	select {
	case r := <-result:
		if r.err == nil {
			t.Fatal("SendFileContext to a reset peer succeeded")
		}
		if r.n <= 0 || r.n >= 64<<20 {
			t.Fatalf("SendFileContext reported %d of its 64 MiB after the peer reset", r.n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SendFileContext kept waiting after the peer reset")
	}
}

// A file truncated while its range waits in the queue fails the stream, and
// the failure closes the connection by itself: a flush whose error its caller
// ignored must not leave a write turn spinning on a failed stream.
func TestSendFileTruncatedInTheQueueClosesTheConnection(t *testing.T) {
	path, data := writeTestFile(t, "trunc-spin", 4<<20)
	closed := make(chan error, 1)
	var once atomic.Bool
	ev := &Events{Handler: FuncHandler{
		Read: func(c Conn) error {
			_, _ = c.Discard(-1)
			if !once.CompareAndSwap(false, true) {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := c.Write([]byte("H")); err != nil {
				return err
			}
			if n, err := SendFile(c, f, 0, int64(len(data))); n != int64(len(data)) || err != nil {
				t.Errorf("SendFile = (%d, %v)", n, err)
			}
			if err := os.Truncate(path, 256<<10); err != nil {
				t.Error(err)
			}
			_ = c.Flush() // the caller ignores the error, as handlers do
			return nil
		},
		Close: func(_ Conn, err error) { closed <- err },
	}}
	got := servePair(t, ev, 16<<10, func(c Conn) {
		// The connection's turn runs the handler; the client reads meanwhile.
		select {
		case err := <-closed:
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("OnClose = %v, want io.ErrUnexpectedEOF", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the connection stayed open after its file was truncated")
			_ = c.Close()
		}
	}, func(cli net.Conn) []byte {
		if _, err := cli.Write([]byte("G")); err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(cli)
		return b
	})
	if len(got) < 256<<10 || !bytes.Equal(got[1:256<<10+1], data[:256<<10]) {
		t.Fatalf("the peer got %d bytes, want the %d the file still held", len(got), 256<<10+1)
	}
}

// Bytes accepted before a Close or CloseWrite that races a direct SendFile
// still reach the peer: the close's drain sends them, and the rest of the
// range behind them.
func TestSendFileRestIsQueuedWhenACloseRacesIt(t *testing.T) {
	for _, graceful := range []bool{true, false} {
		name := "close"
		if graceful {
			name = "closewrite"
		}
		t.Run(name, func(t *testing.T) {
			path, data := writeTestFile(t, "racing", 1<<20)
			tail := []byte("written after the send began")
			went := make(chan struct{})
			var once atomic.Bool
			hook := func(*fdConn) {
				if once.CompareAndSwap(false, true) {
					close(went)
				}
			}
			testHookSendFileDirect.Store(&hook)
			defer testHookSendFileDirect.Store(nil)
			got := sendFilePair(t, &Events{}, nil, false, nil, func(c Conn) {
				f, err := os.Open(path)
				if err != nil {
					t.Error(err)
					return
				}
				defer f.Close()
				sent := make(chan error, 1)
				go func() {
					n, err := SendFile(c, f, 0, int64(len(data)))
					if err == nil && n != int64(len(data)) {
						err = io.ErrShortWrite
					}
					sent <- err
				}()
				<-went
				if _, err := c.Write(tail); err != nil {
					t.Errorf("the write before the close: %v", err)
				}
				if graceful {
					err = c.CloseWrite()
				} else {
					err = c.Close()
				}
				if err != nil {
					t.Errorf("the close: %v", err)
				}
				if err := <-sent; err != nil {
					t.Errorf("SendFile = %v", err)
				}
			})
			want := append(append([]byte(nil), data...), tail...)
			if !bytes.Equal(got, want) {
				t.Fatalf("the peer got %d bytes, want the file and the write before the close (%d)", len(got), len(want))
			}
		})
	}
}
