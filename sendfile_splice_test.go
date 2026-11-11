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
	"sync"
	"testing"
	"time"
)

// patternFile writes size bytes whose values follow their offsets, so a
// misplaced byte shows where it came from.
func patternFile(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*31 + i>>11)
	}
	path := filepath.Join(t.TempDir(), "pattern")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

// dialServed dials a fresh listener of ev, started with startBlockingEvents,
// and returns the client end.
func dialServed(t *testing.T, ev *Events, opts ...ListenOption) net.Conn {
	t.Helper()
	stop := startBlockingEvents(t, ev)
	t.Cleanup(stop)
	ln, err := ev.Listen("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	_ = cli.SetDeadline(time.Now().Add(20 * time.Second))
	return cli
}

type lowerStub struct {
	Lower
	buf bytes.Buffer
}

func (c *lowerStub) WriteOwned(b *Buffer) (int, error) {
	defer ReleaseBuffer(b)
	return c.buf.Write(b.Bytes())
}

// A Lower that is no uio connection gets the range as pooled buffers; the
// range is checked against the file before anything is written.
func TestSendFileToAnyLowerWritesBuffers(t *testing.T) {
	path, data := patternFile(t, 64<<10)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := SendFile(nil, f, 0, 1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SendFile(nil conn) = %v, want ErrUnsupported", err)
	}
	stub := &lowerStub{}
	if _, err := SendFile(stub, nil, 0, 1); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("SendFile(nil file) = %v, want os.ErrInvalid", err)
	}
	if n, err := SendFile(stub, f, 0, 0); n != 0 || err != nil {
		t.Fatalf("SendFile of nothing = (%d, %v)", n, err)
	}
	if _, err := SendFile(stub, f, int64(len(data)), 64); !errors.Is(err, io.ErrUnexpectedEOF) || stub.buf.Len() != 0 {
		t.Fatalf("SendFile past the end = %v with %d bytes written", err, stub.buf.Len())
	}
	if _, err := f.Seek(128, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if n, err := SendFile(stub, f, -1, 1024); n != 1024 || err != nil || !bytes.Equal(stub.buf.Bytes(), data[128:1152]) {
		t.Fatalf("SendFile from the position = (%d, %v)", n, err)
	}
	if pos, _ := f.Seek(0, io.SeekCurrent); pos != 1152 {
		t.Fatalf("position %d after sending 1024 bytes from 128", pos)
	}
	closed, err := os.CreateTemp(t.TempDir(), "closed-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	if _, err := SendFile(stub, closed, -1, 10); err == nil {
		t.Fatal("SendFile from a closed file's position succeeded")
	}
	if _, err := SendFile(stub, closed, 0, -1); err == nil {
		t.Fatal("SendFile of a closed file succeeded")
	}
}

// On a turn, ranges sent in pieces, and the writes before and after them of
// every kind, leave in the order they were made.
func TestSendFileOnATurnKeepsWriteOrder(t *testing.T) {
	path, data := patternFile(t, 512<<10)
	header := []byte("HTTP/1.1 200 OK\r\nContent-Length: 524288\r\n\r\n")
	trailer := []byte("\r\n--END-OF-THE-FILE-RESPONSE--\r\n")
	ev := &Events{MaxOutboundBuffered: 2 << 20}
	ev.Handler = FuncHandler{Read: func(c Conn) error {
		_, _ = c.Discard(-1)
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := c.Write(header); err != nil {
			return err
		}
		half := int64(len(data) / 2)
		if n, err := SendFile(c, f, 0, half); n != half || err != nil {
			t.Errorf("SendFile of the first half = (%d, %v)", n, err)
		}
		if _, err := f.Seek(half, io.SeekStart); err != nil {
			return err
		}
		if n, err := SendFile(c, f, -1, -1); n != half || err != nil {
			t.Errorf("SendFile of the rest = (%d, %v)", n, err)
		}
		if _, err := c.Write(trailer[:8]); err != nil {
			return err
		}
		if _, err := c.Writev([][]byte{trailer[8:16]}); err != nil {
			return err
		}
		tail := AcquireBuffer(len(trailer) - 16)
		_, _ = tail.Write(trailer[16:])
		if _, err := c.WriteOwned(tail); err != nil {
			return err
		}
		return c.Close()
	}}
	cli := dialServed(t, ev)
	if _, err := cli.Write([]byte("GET")); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(cli)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte(nil), header...), data...), trailer...)
	if !bytes.Equal(got, want) {
		t.Fatalf("the peer got %d bytes in another order than header, file and trailer (%d)", len(got), len(want))
	}
}

// The blocking ways to send a file — SendFile with BlockingWrite on, a
// BlockingConn's SendFileContext and SendFile, a BlockingWriter's, and
// io.Copy into one — each stream their part under a window far shorter than
// the file.
func TestBlockingSendFilesStreamAFile(t *testing.T) {
	path, data := patternFile(t, 512<<10)
	quarter := int64(len(data) / 4)
	ev := &Events{BlockingWrite: true, MaxOutboundBuffered: 64 << 10}
	ev.Handler = FuncHandler{Open: func(c Conn) {
		bc, err := NewBlockingConn(c)
		if err != nil {
			_ = c.Abort(err)
			return
		}
		go func() {
			defer c.Close()
			f, err := os.Open(path)
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if n, err := SendFile(c, f, 0, quarter); n != quarter || err != nil {
				t.Errorf("SendFile = (%d, %v)", n, err)
				return
			}
			if n, err := bc.SendFileContext(ctx, f, quarter, quarter/2); n != quarter/2 || err != nil {
				t.Errorf("BlockingConn.SendFileContext = (%d, %v)", n, err)
				return
			}
			if n, err := bc.SendFile(f, quarter+quarter/2, quarter-quarter/2); n != quarter-quarter/2 || err != nil {
				t.Errorf("BlockingConn.SendFile = (%d, %v)", n, err)
				return
			}
			if n, err := bc.Writer().SendFile(f, 2*quarter, quarter); n != quarter || err != nil {
				t.Errorf("BlockingWriter.SendFile = (%d, %v)", n, err)
				return
			}
			if _, err := f.Seek(3*quarter, io.SeekStart); err != nil {
				t.Error(err)
				return
			}
			if n, err := io.Copy(bc.Writer(), f); n != quarter || err != nil {
				t.Errorf("io.Copy = (%d, %v)", n, err)
				return
			}
			_ = bc.Flush()
		}()
	}}
	cli := dialServed(t, ev)
	got, err := io.ReadAll(cli)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("the peer got %d bytes, want the %d-byte file", len(got), len(data))
	}
}

// Through layers, SendFile reads the range into memory: on a turn within its
// budget, and outside the turns with BlockingWrite on through the
// BlockingWriter, in order.
func TestSendFileThroughLayers(t *testing.T) {
	path, data := patternFile(t, 512<<10)
	ev := &Events{BlockingWrite: true, MaxOutboundBuffered: 128 << 10}
	ev.Handler = FuncHandler{Read: func(c Conn) error {
		_, _ = c.Discard(-1)
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := SendFile(c, f, 0, 256<<10); !errors.Is(err, ErrOutboundOverflow) {
			t.Errorf("SendFile past the budget = %v, want ErrOutboundOverflow", err)
		}
		if n, err := SendFile(c, f, 1024, 2048); n != 2048 || err != nil {
			t.Errorf("SendFile on the turn = (%d, %v)", n, err)
		}
		go func() {
			defer c.Close()
			f, err := os.Open(path)
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			if n, err := SendFile(c, f, 3072, int64(len(data))-3072); n != int64(len(data))-3072 || err != nil {
				t.Errorf("blocking SendFile through layers = (%d, %v)", n, err)
			}
		}()
		return nil
	}}
	cli := dialServed(t, ev, WithLayers(PassThroughLayer))
	if _, err := cli.Write([]byte("GO")); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(cli)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data[1024:]) {
		t.Fatalf("the peer got %d bytes, want the %d from offset 1024", len(got), len(data)-1024)
	}
}

// A bounded SpliceTo into a file takes exactly its count after the bytes a
// reader already read, and the stream reads on after it.
func TestSpliceToAFileThenReadOn(t *testing.T) {
	_, payload := patternFile(t, 384<<10)
	header := []byte("PUT /upload HTTP/1.1\r\nContent-Length: 393216\r\n\r\n")
	followUp := []byte("PING AFTER THE BODY")
	out, err := os.Create(filepath.Join(t.TempDir(), "upload"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	ev := &Events{}
	ev.Handler = FuncHandler{Open: func(c Conn) {
		bc, err := NewBlockingConn(c)
		if err != nil {
			_ = c.Abort(err)
			return
		}
		go func() {
			defer c.Close()
			got := make([]byte, len(header))
			if _, err := io.ReadFull(bc, got); err != nil || !bytes.Equal(got, header) {
				t.Errorf("header = %q, %v", got, err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if n, err := bc.SpliceTo(ctx, out, int64(len(payload))); n != int64(len(payload)) || err != nil {
				t.Errorf("SpliceTo the file = (%d, %v)", n, err)
				return
			}
			tail := make([]byte, len(followUp))
			if _, err := io.ReadFull(bc, tail); err != nil || !bytes.Equal(tail, followUp) {
				t.Errorf("follow-up = %q, %v", tail, err)
				return
			}
			_, _ = bc.Write([]byte("OK"))
			_ = bc.Flush()
		}()
	}}
	cli := dialServed(t, ev)
	// The header and the start of the body arrive together, so the reader
	// holds part of the body when SpliceTo starts.
	if _, err := cli.Write(append(append([]byte(nil), header...), payload[:4096]...)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := cli.Write(append(append([]byte(nil), payload[4096:]...), followUp...)); err != nil {
		t.Fatal(err)
	}
	ack, err := io.ReadAll(cli)
	if err != nil || string(ack) != "OK" {
		t.Fatalf("ack = %q, %v", ack, err)
	}
	disk, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk, payload) {
		t.Fatalf("the file holds %d bytes, want the %d-byte body", len(disk), len(payload))
	}
}

// Splice echoes a stream into its own connection, given as a plain Conn, and
// refuses to run on a turn.
func TestSpliceEchoesIntoTheSameConn(t *testing.T) {
	_, payload := patternFile(t, 384<<10)
	if _, err := Splice(context.Background(), nil, nil, 10); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Splice(nil) = %v, want ErrUnsupported", err)
	}
	ev := &Events{AllowHalfOpen: true}
	ev.Handler = FuncHandler{Open: func(c Conn) {
		if _, err := Splice(context.Background(), c, c, 10); !errors.Is(err, ErrBlockingOnTurn) {
			t.Errorf("Splice on a turn = %v, want ErrBlockingOnTurn", err)
		}
		if _, err := NewBlockingReader(c); err != nil {
			t.Error(err)
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := Splice(ctx, c, c, -1); err != nil {
				_ = c.Abort(err)
				return
			}
			bw, _ := NewBlockingWriter(c)
			_ = bw.Flush()
			_ = c.CloseWrite()
		}()
	}}
	cli := dialServed(t, ev)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = cli.Write(payload)
		_ = cli.(*net.TCPConn).CloseWrite()
	}()
	got, err := io.ReadAll(cli)
	wg.Wait()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("echo of %d bytes: got %d, %v", len(payload), len(got), err)
	}
}

// io.Copy from a BlockingReader into a Conn waits for room in the Conn's
// queue, as SpliceTo does, instead of failing once the queue is full.
func TestIOCopyFromABlockingReaderIntoAConnWaitsForRoom(t *testing.T) {
	_, payload := patternFile(t, 4<<20)
	copied := make(chan error, 1)
	ev := &Events{MaxOutboundBuffered: 64 << 10, AllowHalfOpen: true}
	ev.Handler = FuncHandler{Open: func(c Conn) {
		br, err := NewBlockingReader(c)
		if err != nil {
			copied <- err
			return
		}
		go func() {
			n, err := io.Copy(c, br)
			if err == nil && n != int64(len(payload)) {
				err = io.ErrShortWrite
			}
			copied <- err
			bw, _ := NewBlockingWriter(c)
			_ = bw.Flush()
			_ = c.CloseWrite()
		}()
	}}
	cli := dialServed(t, ev)
	go func() {
		_, _ = cli.Write(payload)
		_ = cli.(*net.TCPConn).CloseWrite()
	}()
	got, err := io.ReadAll(cli)
	if cerr := <-copied; cerr != nil {
		t.Fatalf("io.Copy into the Conn: %v", cerr)
	}
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("echo of %d bytes: got %d, %v", len(payload), len(got), err)
	}
}

// BlockingConn.WriteTo, what io.Copy(bc, br) calls, moves the stream into the
// Conn's own writer with backpressure, as BlockingReader.WriteTo does.
func TestBlockingConnWriteToWaitsForRoom(t *testing.T) {
	_, payload := patternFile(t, 2<<20)
	copied := make(chan error, 1)
	ev := &Events{MaxOutboundBuffered: 64 << 10, AllowHalfOpen: true}
	ev.Handler = FuncHandler{Open: func(c Conn) {
		br, err := NewBlockingReader(c)
		if err != nil {
			copied <- err
			return
		}
		bc, err := NewBlockingConn(c)
		if err != nil {
			copied <- err
			return
		}
		if bc.InternalWriter() == nil {
			copied <- ErrUnsupported
			return
		}
		_ = br
		go func() {
			n, err := bc.WriteTo(c)
			if err == nil && n != int64(len(payload)) {
				err = io.ErrShortWrite
			}
			copied <- err
			_ = bc.Flush()
			_ = c.CloseWrite()
		}()
	}}
	cli := dialServed(t, ev)
	go func() {
		_, _ = cli.Write(payload)
		_ = cli.(*net.TCPConn).CloseWrite()
	}()
	got, err := io.ReadAll(cli)
	if cerr := <-copied; cerr != nil {
		t.Fatalf("BlockingConn.WriteTo: %v", cerr)
	}
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("echo of %d bytes: got %d, %v", len(payload), len(got), err)
	}
}
