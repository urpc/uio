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
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// serveFileOps runs ops on a blocking writer of a served connection, then
// closes it, and returns what the client read.
func serveFileOps(t *testing.T, ops func(c Conn, bw *BlockingWriter)) []byte {
	t.Helper()
	ev := &Events{
		Handler: FuncHandler{
			Open: func(c Conn) {
				bw, err := NewBlockingWriter(c)
				if err != nil {
					_ = c.Abort(err)
					return
				}
				go func() {
					ops(c, bw)
					_ = bw.Flush()
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
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	_ = cli.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(cli)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// ReadFrom sends a regular file's fstat size with sendfile(2) and reads the
// rest, and anything without a size, as from any reader.
func TestBlockingWriterReadFromPipesAndFiles(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 10)
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got := serveFileOps(t, func(_ Conn, bw *BlockingWriter) {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Error(err)
			return
		}
		go func() {
			_, _ = pw.Write([]byte("pipe-bytes"))
			_ = pw.Close()
		}()
		if n, err := io.CopyN(bw, pr, 10); n != 10 || err != nil {
			t.Errorf("io.CopyN(pipe, 10) = (%d, %v), want (10, nil)", n, err)
		}
		_ = pr.Close()

		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		if n, err := io.CopyN(bw, f, 150); n != 100 || !errors.Is(err, io.EOF) {
			t.Errorf("io.CopyN(100-byte file, 150) = (%d, %v), want (100, EOF)", n, err)
		}
		if pos, _ := f.Seek(0, io.SeekCurrent); pos != 100 {
			t.Errorf("file position after io.CopyN = %d, want 100", pos)
		}
		_, _ = f.Seek(10, io.SeekStart)
		if n, err := bw.ReadFrom(f); n != 90 || err != nil {
			t.Errorf("ReadFrom(file at 10) = (%d, %v), want (90, nil)", n, err)
		}
	})
	want := append(append([]byte("pipe-bytes"), data...), data[10:]...)
	if !bytes.Equal(got, want) {
		t.Fatalf("client read %q, want %q", got, want)
	}
}

// Synthetic files size what they generate as 0 or a page: SendFile and
// ReadFrom read them to their end instead of trusting fstat.
func TestSendFileReadsSyntheticFilesToTheirEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs and sysfs are Linux file systems")
	}
	sysPath := "/sys/class/net/lo/address"
	sysWant, sysErr := os.ReadFile(sysPath)
	sentCh := make(chan []int64, 1)
	got := serveFileOps(t, func(c Conn, bw *BlockingWriter) {
		var sent []int64
		defer func() { sentCh <- sent }()
		f, err := os.Open("/proc/self/status")
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		n, err := SendFile(c, f, 0, -1)
		if n <= 0 || err != nil {
			t.Errorf("SendFile(/proc/self/status, 0, -1) = (%d, %v)", n, err)
		}
		sent = append(sent, n)
		_, _ = f.Seek(0, io.SeekStart)
		n, err = io.CopyN(bw, f, 16)
		if n != 16 || err != nil {
			t.Errorf("io.CopyN(/proc/self/status, 16) = (%d, %v), want (16, nil)", n, err)
		}
		sent = append(sent, n)
		_, _ = f.Seek(0, io.SeekStart)
		n, err = bw.ReadFrom(f)
		if n <= 0 || err != nil {
			t.Errorf("ReadFrom(/proc/self/status) = (%d, %v)", n, err)
		}
		sent = append(sent, n)
		if sysErr == nil {
			sf, err := os.Open(sysPath)
			if err != nil {
				t.Error(err)
				return
			}
			defer sf.Close()
			n, err = SendFile(c, sf, 0, -1)
			if n != int64(len(sysWant)) || err != nil {
				t.Errorf("SendFile(%s, 0, -1) = (%d, %v), want (%d, nil)", sysPath, n, err, len(sysWant))
			}
			sent = append(sent, n)
		}
	})
	sent := <-sentCh
	var total int64
	for _, n := range sent {
		total += n
	}
	if int64(len(got)) != total || !strings.HasPrefix(string(got), "Name:") {
		t.Fatalf("client read %d bytes starting %q, server sent %v", len(got), got[:min(len(got), 16)], sent)
	}
	if sysErr == nil && !bytes.HasSuffix(got, sysWant) {
		t.Fatalf("client read ends %q, want %q", got[max(len(got)-len(sysWant), 0):], sysWant)
	}
}
