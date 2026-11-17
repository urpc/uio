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

package filerange

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func tempFile(t *testing.T, size int) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// A regular file's range is sized by fstat: a count runs no further than the
// file, and a negative count runs to its end.
func TestResolveSizesARegularFile(t *testing.T) {
	f := tempFile(t, 1000)
	r, err := Resolve(f, 100, 200)
	if err != nil || r.Start != 100 || r.Len != 200 || !r.Native {
		t.Fatalf("Resolve(100, 200) = %+v, %v", r, err)
	}
	if r.Dev == 0 && r.Ino == 0 && runtime.GOOS != "windows" {
		t.Fatalf("a native range carries no file identity: %+v", r)
	}
	if r, err = Resolve(f, 100, -1); err != nil || r.Len != 900 || !r.Native {
		t.Fatalf("Resolve(100, -1) = %+v, %v", r, err)
	}
	if r, err = Resolve(f, 2000, -1); err != nil || r.Len != 0 {
		t.Fatalf("Resolve past the end, to the end = %+v, %v; want an empty range", r, err)
	}
	if _, err = Resolve(f, 900, 101); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Resolve(900, 101) on 1000 bytes = %v, want io.ErrUnexpectedEOF", err)
	}
	if _, err = Resolve(nil, 0, 1); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("Resolve(nil) = %v, want os.ErrInvalid", err)
	}
}

// A range from the file's position starts there, and Finish moves the position
// past what was sent; a range from an offset leaves the position alone.
func TestResolveFromThePosition(t *testing.T) {
	f := tempFile(t, 1000)
	if _, err := f.Seek(300, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(f, -1, -1)
	if err != nil || r.Start != 300 || r.Len != 700 {
		t.Fatalf("Resolve(-1, -1) at 300 = %+v, %v", r, err)
	}
	r.Finish(f, 50)
	if pos, _ := f.Seek(0, io.SeekCurrent); pos != 350 {
		t.Fatalf("position after Finish(50) = %d, want 350", pos)
	}
	r, _ = Resolve(f, 10, 20)
	r.Finish(f, 20)
	if pos, _ := f.Seek(0, io.SeekCurrent); pos != 350 {
		t.Fatalf("a range from an offset moved the position to %d", pos)
	}
}

// A pipe has no length: its range is read to its end, never sent natively,
// and resolving it leaves the pipe's mode alone.
func TestResolveReadsAPipeToItsEnd(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	if r, err := Resolve(pr, 0, -1); err != nil || r.Len != -1 || r.Native {
		t.Fatalf("Resolve(pipe, 0, -1) = %+v, %v", r, err)
	}
	if r, err := Resolve(pr, 0, 5); err != nil || r.Len != 5 || r.Native {
		t.Fatalf("Resolve(pipe, 0, 5) = %+v, %v", r, err)
	}
	if _, ok := Span(pr, -1); ok {
		t.Fatal("Span offered a pipe to sendfile(2)")
	}
}

// Span offers what fstat sizes past the position, within a limit.
func TestSpanIsTheSizedRestWithinALimit(t *testing.T) {
	f := tempFile(t, 1000)
	if _, err := f.Seek(400, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if r, ok := Span(f, -1); !ok || r.Start != 400 || r.Len != 600 {
		t.Fatalf("Span(-1) = %+v, %v", r, ok)
	}
	if r, ok := Span(f, 100); !ok || r.Len != 100 {
		t.Fatalf("Span(100) = %+v, %v", r, ok)
	}
	if _, ok := Span(f, 0); ok {
		t.Fatal("Span(0) offered a range")
	}
	if _, err := f.Seek(1000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, ok := Span(f, -1); ok {
		t.Fatal("Span at the end offered a range")
	}
}

// An empty transfer succeeds from any offset, past the end of the file or not,
// as an empty one always does; a count that only looks past the end because
// its sum with the offset wraps is still refused.
func TestResolveEmptyAndWrappingRanges(t *testing.T) {
	f := tempFile(t, 1000)
	for _, off := range []int64{0, 999, 1000, 5000} {
		if r, err := Resolve(f, off, 0); err != nil || r.Len != 0 {
			t.Fatalf("Resolve(%d, 0) = %+v, %v; want an empty range", off, r, err)
		}
	}
	if _, err := Resolve(f, 100, math.MaxInt64-50); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Resolve(100, MaxInt64-50) = %v, want io.ErrUnexpectedEOF", err)
	}
	if _, err := Resolve(f, 1001, 1); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Resolve(1001, 1) = %v, want io.ErrUnexpectedEOF", err)
	}
	if r, err := Resolve(f, 990, 10); err != nil || r.Len != 10 {
		t.Fatalf("Resolve(990, 10) = %+v, %v; want the last 10 bytes", r, err)
	}
}

// A file whose Stat fails reports the error before any range is formed.
func TestResolveStatError(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "closed-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := Resolve(f, 0, 1); err == nil {
		t.Fatal("Resolve on a closed file succeeded, want a Stat error")
	}
}
