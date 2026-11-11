//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

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

package socket

import (
	"bytes"
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
)

func TestSendfileAndSpliceContracts(t *testing.T) {
	n, err := Sendfile(-1, -1, nil, 0)
	if n != 0 || err != nil {
		t.Fatalf("Sendfile(count=0) = (%d, %v), want (0, nil)", n, err)
	}

	tmp, err := os.CreateTemp(t.TempDir(), "sendfile-*")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	payload := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	if _, err := tmp.Write(payload); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			accepted <- c
		}
	}()

	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	srv := <-accepted
	defer srv.Close()

	var srvFD int
	sc, err := srv.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	_ = sc.Control(func(fd uintptr) { srvFD = int(fd) })

	var inFD int
	fsc, err := tmp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	_ = fsc.Control(func(fd uintptr) { inFD = int(fd) })

	off := int64(16)
	want := len(payload) - 16
	sent := 0
	done := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(cli)
		done <- buf
	}()

	for sent < want {
		nw, serr := Sendfile(srvFD, inFD, &off, want-sent)
		if nw > 0 {
			sent += nw
		}
		if serr != nil {
			if serr == syscall.EAGAIN || serr == syscall.EWOULDBLOCK {
				continue
			}
			if IsSendfileUnsupported(serr) {
				t.Skipf("sendfile unsupported on this kernel: %v", serr)
			}
			t.Fatalf("Sendfile error: %v", serr)
		}
	}
	if off != int64(len(payload)) {
		t.Fatalf("Sendfile final offset = %d, want %d", off, len(payload))
	}
	_ = srv.Close()
	got := <-done
	if !bytes.Equal(got, payload[16:]) {
		t.Fatalf("Sendfile payload mismatch: got %d bytes, want %d", len(got), want)
	}

	// Verify IsSendfileUnsupported classification.
	for _, e := range []error{syscall.ENOSYS, syscall.EINVAL, syscall.EOPNOTSUPP, syscall.ENOTSUP, syscall.ENOTSOCK, syscall.ESPIPE, syscall.ENODEV} {
		if !IsSendfileUnsupported(e) {
			t.Fatalf("IsSendfileUnsupported(%v) = false, want true", e)
		}
	}
	if IsSendfileUnsupported(syscall.EPIPE) {
		t.Fatal("IsSendfileUnsupported(EPIPE) = true, want false")
	}

	// Verify Splice helpers on both Linux and non-Linux.
	if n, err := Splice(-1, nil, -1, nil, 0); n != 0 || err != nil {
		t.Fatalf("Splice(count=0) = (%d, %v)", n, err)
	}
	pipe, perr := AcquireSplicePipe()
	if runtime.GOOS == "linux" {
		if perr != nil {
			t.Fatalf("AcquireSplicePipe on linux: %v", perr)
		}
		ReleaseSplicePipe(pipe)
		pipe2, perr2 := AcquireSplicePipe()
		if perr2 != nil {
			t.Fatalf("AcquireSplicePipe reuse: %v", perr2)
		}
		CloseSplicePipe(pipe2)
		if !IsSpliceUnsupported(syscall.EINVAL) || IsSpliceUnsupported(syscall.EPIPE) {
			t.Fatal("unexpected IsSpliceUnsupported classification")
		}
	} else {
		if perr == nil {
			t.Fatal("expected error from AcquireSplicePipe on non-Linux")
		}
		ReleaseSplicePipe(pipe)
		CloseSplicePipe(pipe)
		if _, rerr := ReadSplicePipe(-1, make([]byte, 8)); rerr == nil {
			t.Fatal("expected error from ReadSplicePipe on non-Linux")
		}
		if _, serr := Splice(-1, nil, -1, nil, 16); serr == nil || !IsSpliceUnsupported(serr) {
			t.Fatalf("expected unsupported error from Splice on non-Linux, got %v", serr)
		}
	}
}
