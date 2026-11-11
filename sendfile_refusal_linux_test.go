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
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// appendOnly makes the connection's socket refuse sendfile(2) on Linux — the
// kernel's split of a file into a socket answers EINVAL for one opened
// O_APPEND — while send(2) goes on working.
func appendOnly(c Conn) {
	fd := int(socketOfConn(c).fd)
	flags, _, _ := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFL, 0)
	_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFL, flags|syscall.O_APPEND)
}

// A socket sendfile(2) refuses still gets its file: the range is queued and
// sent with reads, for SendFile, for a BlockingWriter's SendFileContext and
// for io.Copy into one alike.
func TestSendFileRefusedByTheKernelSendsWithReads(t *testing.T) {
	path, data := writeTestFile(t, "refused", 8<<20)
	t.Run("sendfile", func(t *testing.T) {
		sent := make(chan error, 1)
		got := servePair(t, &Events{}, 16<<10, func(c Conn) {
			appendOnly(c)
			f, err := os.Open(path)
			if err != nil {
				sent <- err
				return
			}
			defer f.Close()
			awaitOpened(c)
			n, err := SendFile(c, f, 0, -1)
			if err == nil && n != int64(len(data)) {
				err = io.ErrShortWrite
			}
			sent <- err
			_ = c.Close()
		}, func(cli net.Conn) []byte {
			b, _ := io.ReadAll(cli)
			return b
		})
		if err := <-sent; err != nil {
			t.Fatalf("SendFile = %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("the peer got %d bytes, want the %d-byte file", len(got), len(data))
		}
	})
	t.Run("blocking", func(t *testing.T) {
		sent := make(chan error, 1)
		got := servePair(t, &Events{}, 16<<10, func(c Conn) {
			appendOnly(c)
			f, err := os.Open(path)
			if err != nil {
				sent <- err
				return
			}
			defer f.Close()
			bw, err := NewBlockingWriter(c, WithBufferSize(256<<10))
			if err != nil {
				sent <- err
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			n, err := bw.SendFileContext(ctx, f, 0, -1)
			if err == nil && n != int64(len(data)) {
				err = io.ErrShortWrite
			}
			sent <- err
			_ = c.Close()
		}, func(cli net.Conn) []byte {
			b, _ := io.ReadAll(cli)
			return b
		})
		if err := <-sent; err != nil {
			t.Fatalf("SendFileContext = %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("the peer got %d bytes, want the %d-byte file", len(got), len(data))
		}
	})
	t.Run("io.Copy", func(t *testing.T) {
		sent := make(chan error, 1)
		var copied atomic.Int64
		got := servePair(t, &Events{}, 16<<10, func(c Conn) {
			appendOnly(c)
			f, err := os.Open(path)
			if err != nil {
				sent <- err
				return
			}
			defer f.Close()
			bw, err := NewBlockingWriter(c, WithBufferSize(256<<10))
			if err != nil {
				sent <- err
				return
			}
			n, err := io.Copy(bw, f)
			copied.Store(n)
			sent <- err
			_ = c.Close()
		}, func(cli net.Conn) []byte {
			b, _ := io.ReadAll(cli)
			return b
		})
		if err := <-sent; err != nil {
			t.Fatalf("io.Copy = %v", err)
		}
		if copied.Load() != int64(len(data)) || !bytes.Equal(got, data) {
			t.Fatalf("io.Copy moved %d bytes; the peer got %d of %d", copied.Load(), len(got), len(data))
		}
	})
}

// A file that cannot be read fails the SendFile before it is queued, whether
// or not something else is queued, and the connection outlives it.
func TestSendFileOfAnUnreadableFileFailsTheCallOnly(t *testing.T) {
	path, _ := writeTestFile(t, "wronly", 1<<20)
	first, second := make(chan error, 1), make(chan error, 1)
	got := servePair(t, &Events{}, 16<<10, func(c Conn) {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		awaitOpened(c)
		n, err := SendFile(c, f, 0, -1)
		if n != 0 {
			t.Errorf("SendFile of a write-only file reported %d bytes", n)
		}
		first <- err
		// A range behind queued output takes the queue path.
		_, _ = c.Write(bytes.Repeat([]byte{'Q'}, 64<<10))
		n, err = SendFile(c, f, 0, -1)
		if n != 0 {
			t.Errorf("queued SendFile of a write-only file reported %d bytes", n)
		}
		second <- err
		_ = c.Close()
	}, func(cli net.Conn) []byte {
		b, _ := io.ReadAll(cli)
		return b
	})
	if err := <-first; err == nil {
		t.Fatal("SendFile of a write-only file succeeded on an empty queue")
	}
	if err := <-second; err == nil {
		t.Fatal("SendFile of a write-only file succeeded behind queued output")
	}
	if len(got) != 64<<10 || !bytes.Equal(got, bytes.Repeat([]byte{'Q'}, 64<<10)) {
		t.Fatalf("the peer got %d bytes, want only the write before the failed SendFiles", len(got))
	}
}
