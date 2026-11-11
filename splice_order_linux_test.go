//go:build linux && !stdio

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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/socket"
)

// offsetPattern fills n bytes with 8-byte words that each hold their own
// offset, so a misplaced run names where it came from.
func offsetPattern(n int) []byte {
	b := make([]byte, n)
	for i := 0; i+8 <= n; i += 8 {
		binary.LittleEndian.PutUint64(b[i:], uint64(i))
	}
	return b
}

func checkOffsetPattern(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	at := 0
	for at < min(len(got), len(want)) && got[at] == want[at] {
		at++
	}
	word := at / 8 * 8
	if word+8 <= len(got) {
		t.Errorf("%s: %d of %d bytes, first difference at %d holds offset %d",
			name, len(got), len(want), at, binary.LittleEndian.Uint64(got[word:]))
		return
	}
	t.Errorf("%s: %d of %d bytes, first difference at %d", name, len(got), len(want), at)
}

// requireSpliced fails a test whose transfers moved no byte with splice(2),
// unless the kernel refused this user a pipe since refusals was read: past
// pipe-user-pages-soft its pipes are too small, and SpliceTo copies by
// design. Test processes running side by side share that budget.
func requireSpliced(t *testing.T, moved int64, refusals int64) {
	t.Helper()
	if moved > 0 {
		return
	}
	if socket.SmallPipeRefusals() > refusals {
		t.Skip("the kernel gave this user's splices no full-size pipe")
	}
	t.Error("no bytes went through splice(2)")
}

// spliceCount counts the bytes splice(2) moves out of sockets while a test
// runs, and the pipes the kernel refused meanwhile.
type spliceCount struct {
	moved    atomic.Int64
	refusals int64
}

func countSpliceReads(t *testing.T) *spliceCount {
	t.Helper()
	c := &spliceCount{refusals: socket.SmallPipeRefusals()}
	hook := func(_ *fdConn, n int) { c.moved.Add(int64(n)) }
	testHookSpliceRead.Store(&hook)
	t.Cleanup(func() { testHookSpliceRead.Store(nil) })
	return c
}

// Load reports the bytes moved so far.
func (c *spliceCount) Load() int64 { return c.moved.Load() }

// require fails the test if nothing moved with splice(2); see requireSpliced.
func (c *spliceCount) require(t *testing.T, since int64) {
	t.Helper()
	requireSpliced(t, c.moved.Load()-since, c.refusals)
}

type spliceResult struct {
	name string
	n    int64
	err  error
}

// runSpliceToFiles serves conns connections. open attaches the blocking
// reader in OnOpen, before any byte is delivered, and returns the transfer the
// server starts into a file after delay; each client sends with send.
func runSpliceToFiles(t *testing.T, conns int, delay time.Duration, halfOpen bool,
	open func(c Conn) (func(f *os.File) (int64, error), error), send func(cli *net.TCPConn) error) []spliceResult {
	t.Helper()
	dir := t.TempDir()
	results := make(chan spliceResult, conns)
	var seq atomic.Int32
	ev := &Events{
		AllowHalfOpen: halfOpen,
		Handler: FuncHandler{
			Open: func(c Conn) {
				name := filepath.Join(dir, fmt.Sprintf("out-%d", seq.Add(1)))
				move, err := open(c)
				if err != nil {
					results <- spliceResult{name, 0, err}
					return
				}
				go func() {
					f, err := os.Create(name)
					if err != nil {
						results <- spliceResult{name, 0, err}
						return
					}
					defer f.Close()
					time.Sleep(delay)
					n, err := move(f)
					results <- spliceResult{name, n, err}
					_ = c.Close()
				}()
			},
		},
	}
	stop := startBlockingEvents(t, ev)
	defer stop()
	ln, err := ev.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < conns; i++ {
		go func() {
			raw, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Error(err)
				return
			}
			cli := raw.(*net.TCPConn)
			defer cli.Close()
			if err := send(cli); err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, cli)
		}()
	}
	out := make([]spliceResult, 0, conns)
	for i := 0; i < conns; i++ {
		select {
		case r := <-results:
			out = append(out, r)
		case <-time.After(30 * time.Second):
			t.Fatalf("only %d of %d transfers finished", i, conns)
		}
	}
	return out
}

// A blocking SpliceTo takes the socket from the connection's turns. Here the
// reader's window fills first, so the turns hold bytes the splice must not
// overtake, and the transport resumes as SpliceTo drains them. Small
// GOMAXPROCS values are where the splice goroutine and the turns overlap.
func TestSpliceToKeepsOrderAfterHeldWindow(t *testing.T) {
	const size = 8 << 20
	want := offsetPattern(size)
	for _, procs := range []int{2, 4} {
		t.Run(fmt.Sprintf("procs=%d", procs), func(t *testing.T) {
			defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(procs))
			moved := countSpliceReads(t)
			results := runSpliceToFiles(t, 8, 100*time.Millisecond, false,
				func(c Conn) (func(*os.File) (int64, error), error) {
					bc, err := NewBlockingConn(c, WithBufferSize(64<<10))
					return func(f *os.File) (int64, error) {
						ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						return bc.SpliceTo(ctx, f, size)
					}, err
				},
				func(cli *net.TCPConn) error {
					_, err := cli.Write(want)
					return err
				})
			for _, r := range results {
				if r.err != nil || r.n != size {
					t.Errorf("%s: SpliceTo = (%d, %v), want (%d, nil)", r.name, r.n, r.err, size)
				}
				got, err := os.ReadFile(r.name)
				if err != nil {
					t.Fatal(err)
				}
				checkOffsetPattern(t, r.name, got, want)
			}
			moved.require(t, 0)
		})
	}
}

// io.Copy from a BlockingReader reaches the same splice path through WriteTo.
func TestIOCopyFromBlockingReaderKeepsOrder(t *testing.T) {
	const size = 8 << 20
	want := offsetPattern(size)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	moved := countSpliceReads(t)
	results := runSpliceToFiles(t, 8, 100*time.Millisecond, true,
		func(c Conn) (func(*os.File) (int64, error), error) {
			br, err := NewBlockingReader(c, WithBufferSize(64<<10))
			return func(f *os.File) (int64, error) { return io.Copy(f, br) }, err
		},
		func(cli *net.TCPConn) error {
			if _, err := cli.Write(want); err != nil {
				return err
			}
			return cli.CloseWrite()
		})
	for _, r := range results {
		if r.err != nil || r.n != size {
			t.Errorf("%s: io.Copy = (%d, %v), want (%d, nil)", r.name, r.n, r.err, size)
		}
		got, err := os.ReadFile(r.name)
		if err != nil {
			t.Fatal(err)
		}
		checkOffsetPattern(t, r.name, got, want)
	}
	moved.require(t, 0)
}

// SpliceTo running while the stream arrives in pieces of every size.
func TestSpliceToWhileStreamArrives(t *testing.T) {
	const size = 4 << 20
	want := offsetPattern(size)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	moved := countSpliceReads(t)
	var seed atomic.Int64
	results := runSpliceToFiles(t, 8, 0, false,
		func(c Conn) (func(*os.File) (int64, error), error) {
			bc, err := NewBlockingConn(c)
			return func(f *os.File) (int64, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				return bc.SpliceTo(ctx, f, size)
			}, err
		},
		func(cli *net.TCPConn) error {
			_ = cli.SetNoDelay(true)
			rng := rand.New(rand.NewSource(seed.Add(1)))
			for off := 0; off < size; {
				n := min(size-off, 256+rng.Intn(16<<10))
				if _, err := cli.Write(want[off : off+n]); err != nil {
					return err
				}
				off += n
				if rng.Intn(8) == 0 {
					time.Sleep(time.Duration(rng.Intn(200)) * time.Microsecond)
				}
			}
			return nil
		})
	for _, r := range results {
		if r.err != nil || r.n != size {
			t.Errorf("%s: SpliceTo = (%d, %v), want (%d, nil)", r.name, r.n, r.err, size)
		}
		got, err := os.ReadFile(r.name)
		if err != nil {
			t.Fatal(err)
		}
		checkOffsetPattern(t, r.name, got, want)
	}
	moved.require(t, 0)
}

// The close of a connection whose socket a SpliceTo owns waits for the
// splice(2) in progress: the descriptor, and so its number, stays the
// socket's until the call has left.
func TestSpliceGateHoldsDescriptorClose(t *testing.T) {
	type opened struct {
		conn Conn
		fd   int
		gate *descriptorGate
	}
	openedCh := make(chan opened, 1)
	ev := &Events{
		Handler: FuncHandler{
			Open: func(c Conn) {
				fdc := c.(*fdConn)
				zc := fdc.zeroCopyState()
				if !zc.spliceGate.enter() {
					t.Error("gate closed on a new connection")
				}
				openedCh <- opened{c, int(fdc.fd), &zc.spliceGate}
			},
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
	o := <-openedCh
	left := false
	defer func() {
		if !left {
			o.gate.leave() // lets the turn holding the close go
		}
	}()
	link := fmt.Sprintf("/proc/self/fd/%d", o.fd)
	before, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	_ = o.conn.Abort(errors.New("test abort"))
	time.Sleep(50 * time.Millisecond)
	if now, err := os.Readlink(link); err != nil || now != before {
		t.Fatalf("descriptor %d closed under a splice in progress: %q -> %q (%v)", o.fd, before, now, err)
	}
	left = true
	o.gate.leave()
	deadline := time.Now().Add(2 * time.Second)
	for {
		now, err := os.Readlink(link)
		if err != nil || now != before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descriptor %d still open after the splice left", o.fd)
		}
		time.Sleep(time.Millisecond)
	}
	if o.gate.enter() {
		t.Fatal("gate admits a splice after the close")
	}
}

// What a blocking SpliceTo takes from the socket reaches OnBytesRead on the
// connection's turns, never on the SpliceTo's goroutine, and every byte of it
// does.
func TestSpliceToReportsWhatItReadOnTheTurns(t *testing.T) {
	const size = 4 << 20
	moved := countSpliceReads(t)
	var reported atomic.Int64
	var offTurn atomic.Int32
	ev := &Events{Observer: FuncHandler{BytesRead: func(_ Conn, n int) {
		reported.Add(int64(n))
		if !OnTurn() {
			offTurn.Add(1)
		}
	}}}
	closed := make(chan struct{})
	result := make(chan error, 1)
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	ev.Handler = FuncHandler{
		Open: func(c Conn) {
			br, err := NewBlockingReader(c)
			if err != nil {
				result <- err
				return
			}
			go func() {
				n, err := br.SpliceTo(context.Background(), out, size)
				if err == nil && n != size {
					err = io.ErrShortWrite
				}
				result <- err
				_ = c.Close()
			}()
		},
		Close: func(Conn, error) { close(closed) },
	}
	cli := dialServed(t, ev)
	if _, err := cli.Write(offsetPattern(size)); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	<-closed
	moved.require(t, 0)
	if got := reported.Load(); got != size {
		t.Fatalf("OnBytesRead reported %d bytes, want %d", got, size)
	}
	if n := offTurn.Load(); n != 0 {
		t.Fatalf("OnBytesRead ran %d times off the turns", n)
	}
}

// A SpliceTo right after another takes the socket again with splice(2), even
// before a read round handed it back, and the stream keeps its order.
func TestSpliceToBackToBackStaysNative(t *testing.T) {
	const part = 2 << 20
	want := offsetPattern(2 * part)
	moved := countSpliceReads(t)
	result := make(chan error, 1)
	var afterFirst atomic.Int64
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	ev := &Events{Handler: FuncHandler{Open: func(c Conn) {
		br, err := NewBlockingReader(c)
		if err != nil {
			result <- err
			return
		}
		go func() {
			defer c.Close()
			if _, err := br.SpliceTo(context.Background(), out, part); err != nil {
				result <- err
				return
			}
			afterFirst.Store(moved.Load())
			_, err := br.SpliceTo(context.Background(), out, part)
			result <- err
		}()
	}}}
	cli := dialServed(t, ev)
	if _, err := cli.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	moved.require(t, afterFirst.Load())
	got, _ := os.ReadFile(out.Name())
	checkOffsetPattern(t, "two SpliceTo calls", got, want)
}

// The end of the stream a SpliceTo meets is passed on by the connection's
// turns: SpliceTo up to the end succeeds, and the connection closes as one
// whose peer ended does.
func TestSpliceToLeavesTheEndOfStreamToTheTurns(t *testing.T) {
	const size = 1 << 20
	want := offsetPattern(size)
	result := make(chan spliceResult, 1)
	closed := make(chan error, 1)
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	ev := &Events{Handler: FuncHandler{
		Open: func(c Conn) {
			br, err := NewBlockingReader(c)
			if err != nil {
				result <- spliceResult{err: err}
				return
			}
			go func() {
				n, err := br.SpliceTo(context.Background(), out, -1)
				result <- spliceResult{n: n, err: err}
			}()
		},
		Close: func(_ Conn, err error) { closed <- err },
	}}
	cli := dialServed(t, ev)
	if _, err := cli.Write(want); err != nil {
		t.Fatal(err)
	}
	_ = cli.(*net.TCPConn).CloseWrite()
	r := <-result
	if r.err != nil || r.n != size {
		t.Fatalf("SpliceTo up to the end = (%d, %v), want (%d, nil)", r.n, r.err, size)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("OnClose = %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stayed open after its peer ended")
	}
	got, _ := os.ReadFile(out.Name())
	checkOffsetPattern(t, "SpliceTo to the end", got, want)
}
