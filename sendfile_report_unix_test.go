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
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// serveOnRead serves one connection whose socket buffers are small and runs
// onRead for the request the client sends, then returns what the client read
// until the server closed.
func serveOnRead(t *testing.T, ev *Events, onRead func(c Conn)) []byte {
	t.Helper()
	var once atomic.Bool
	ev.Handler = FuncHandler{
		Open: func(c Conn) {
			_ = syscall.SetsockoptInt(int(socketOfConn(c).fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, 16<<10)
		},
		Read: func(c Conn) error {
			_, _ = c.Discard(-1)
			if once.CompareAndSwap(false, true) {
				onRead(c)
			}
			return nil
		},
	}
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
	if _, err := cli.Write([]byte("GET")); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(10 * time.Second))
	b, err := io.ReadAll(cli)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	return b
}

// awaitOpened waits until c's OnOpen returned, after which a SendFile from
// another goroutine may send directly.
func awaitOpened(c Conn) {
	for socketOfConn(c).writeState.Load()&writeOpenedFlag == 0 {
		time.Sleep(time.Millisecond)
	}
}

// directSends counts the SendFiles that sent part of their range from their
// caller's goroutine, while the test runs.
func directSends(t *testing.T) *atomic.Int32 {
	var n atomic.Int32
	hook := func(*fdConn) { n.Add(1) }
	testHookSendFileDirect.Store(&hook)
	t.Cleanup(func() { testHookSendFileDirect.Store(nil) })
	return &n
}

// On a turn, SendFile queues its range behind the turn's replies, so an
// observer that closes the connection once its queue is empty sees that only
// after the whole file left.
func TestSendFileOnATurnNeverShowsAnEmptyQueueMidRange(t *testing.T) {
	path, data := writeTestFile(t, "observer", 4<<20)
	var armed atomic.Bool
	ev := &Events{Observer: FuncHandler{BytesWritten: func(c Conn, n int) {
		if armed.Load() && c.OutboundBuffered() == 0 && armed.CompareAndSwap(true, false) {
			_ = c.Close()
		}
	}}}
	got := serveOnRead(t, ev, func(c Conn) {
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		armed.Store(true)
		if n, err := SendFile(c, f, 0, int64(len(data))); n != int64(len(data)) || err != nil {
			t.Errorf("SendFile = (%d, %v)", n, err)
		}
	})
	if !bytes.Equal(got, data) {
		t.Fatalf("the peer got %d of %d bytes: the observer closed in the middle of the file", len(got), len(data))
	}
}

// What OnBytesWritten writes while a file is being sent leaves after the
// file, never inside it.
func TestWriteInOnBytesWrittenFollowsTheFile(t *testing.T) {
	path, data := writeTestFile(t, "split", 4<<20)
	headers := []byte("HTTP/1.1 200 OK\r\nContent-Length: 4194304\r\n\r\n")
	note := []byte("<<WRITTEN IN OnBytesWritten>>")
	var armed atomic.Bool
	ev := &Events{Observer: FuncHandler{BytesWritten: func(c Conn, n int) {
		// The first report of file bytes: the headers may leave alone first.
		if n != len(headers) && armed.CompareAndSwap(true, false) {
			if _, err := c.Write(note); err != nil {
				t.Errorf("Write in OnBytesWritten: %v", err)
			}
			_ = c.Close()
		}
	}}}
	got := serveOnRead(t, ev, func(c Conn) {
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		_, _ = c.Write(headers)
		armed.Store(true)
		if n, err := SendFile(c, f, 0, int64(len(data))); n != int64(len(data)) || err != nil {
			t.Errorf("SendFile = (%d, %v)", n, err)
		}
	})
	want := append(append(append([]byte(nil), headers...), data...), note...)
	if !bytes.Equal(got, want) {
		t.Fatalf("the peer got %d bytes, want headers, file and note (%d); the note is at %d",
			len(got), len(want), bytes.Index(got, note))
	}
}

// A SendFile outside the turns that sends part of its range from the caller's
// goroutine reports that part from a write turn: OnBytesWritten runs on a
// turn, where a Write never blocks, so with BlockingWrite on it cannot wait
// for the BlockingWriter its caller holds. Every byte is reported.
func TestOnBytesWrittenOfADirectSendRunsOnAWriteTurn(t *testing.T) {
	path, data := writeTestFile(t, "direct", 4<<20)
	var armed, onTurn atomic.Bool
	var reported atomic.Int64
	wrote := make(chan struct{})
	closed := make(chan struct{})
	ev := &Events{
		BlockingWrite:       true,
		MaxOutboundBuffered: 1 << 20,
		Observer: FuncHandler{BytesWritten: func(c Conn, n int) {
			reported.Add(int64(n))
			if armed.CompareAndSwap(true, false) {
				onTurn.Store(OnTurn())
				// The queue holds the rest of the file, over the limit: the
				// write is refused rather than waited for.
				_, _ = c.Write([]byte("x"))
				close(wrote)
			}
		}},
		Handler: closeHandler(func(error) { close(closed) }),
	}
	sent := make(chan error, 1)
	direct := directSends(t)
	got := servePair(t, ev, 16<<10, func(c Conn) {
		f, err := os.Open(path)
		if err != nil {
			sent <- err
			return
		}
		defer f.Close()
		awaitOpened(c)
		armed.Store(true)
		_, err = SendFile(c, f, 0, int64(len(data)))
		sent <- err
		<-wrote
		_ = c.Close()
	}, func(cli net.Conn) []byte {
		var out []byte
		readDone := make(chan struct{})
		go func() {
			out, _ = io.ReadAll(cli)
			close(readDone)
		}()
		select {
		case err := <-sent:
			if err != nil {
				t.Errorf("SendFile: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("SendFile hung: OnBytesWritten ran on its caller's goroutine")
		}
		<-readDone
		return out
	})
	<-closed
	if direct.Load() == 0 {
		t.Fatal("SendFile sent nothing from its caller's goroutine")
	}
	if !onTurn.Load() {
		t.Error("OnBytesWritten of a direct send ran off the turns")
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("the peer got %d bytes, want the %d of the file", len(got), len(data))
	}
	if n := reported.Load(); n != int64(len(data)) {
		t.Fatalf("OnBytesWritten reported %d bytes, want %d", n, len(data))
	}
}

// An observer may take a lock its SendFile's caller holds: the caller never
// runs the observer itself.
func TestSendFileCallerMayHoldALockTheObserverTakes(t *testing.T) {
	path, data := writeTestFile(t, "locked", 1<<20)
	var mu sync.Mutex
	ev := &Events{Observer: FuncHandler{BytesWritten: func(Conn, int) {
		mu.Lock()
		mu.Unlock() //nolint:staticcheck // the lock is the point
	}}}
	sent := make(chan error, 1)
	direct := directSends(t)
	got := servePair(t, ev, 16<<10, func(c Conn) {
		f, err := os.Open(path)
		if err != nil {
			sent <- err
			return
		}
		defer f.Close()
		awaitOpened(c)
		mu.Lock()
		_, err = SendFile(c, f, 0, int64(len(data)))
		mu.Unlock()
		sent <- err
		_ = c.Close()
	}, func(cli net.Conn) []byte {
		select {
		case err := <-sent:
			if err != nil {
				t.Errorf("SendFile: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("SendFile hung while its caller held the observer's lock")
		}
		b, _ := io.ReadAll(cli)
		return b
	})
	if !bytes.Equal(got, data) {
		t.Fatalf("the peer got %d of %d bytes", len(got), len(data))
	}
	if direct.Load() == 0 {
		t.Fatal("SendFile sent nothing from its caller's goroutine")
	}
}

// OnBytesWritten of a direct send on the connection's own turn runs as a
// flush's does: a Flush the callback makes returns at once instead of
// reporting inside the report.
func TestOnBytesWrittenOfATurnDirectSendDoesNotNest(t *testing.T) {
	path, data := writeTestFile(t, "nest", 4<<20)
	var depth, nested atomic.Int32
	ev := &Events{Observer: FuncHandler{BytesWritten: func(c Conn, n int) {
		if d := depth.Add(1); d > 1 {
			nested.Store(d)
		} else if n > 64 {
			_ = c.Flush()
		}
		depth.Add(-1)
	}}}
	got := serveOnRead(t, ev, func(c Conn) {
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		if _, err := c.Write(bytes.Repeat([]byte{'H'}, 64)); err != nil {
			t.Error(err)
		}
		if n, err := SendFile(c, f, 0, int64(len(data))); n != int64(len(data)) || err != nil {
			t.Errorf("SendFile = (%d, %v)", n, err)
		}
		_ = c.Close()
	})
	if d := nested.Load(); d > 1 {
		t.Fatalf("OnBytesWritten reported inside itself, %d deep", d)
	}
	if len(got) < 64 || !bytes.Equal(got[64:], data[:len(got)-64]) || !bytes.Equal(got[:64], bytes.Repeat([]byte{'H'}, 64)) {
		t.Fatalf("the peer got %d bytes, want the header then the file", len(got))
	}
}
