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

package blockio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

func newPulledReader(t *testing.T, window int) (*fakeSource, *Reader) {
	t.Helper()
	src := newFakeSource(window, window)
	src.state = NewState(src)
	r, err := src.state.Reader(window)
	if err != nil {
		t.Fatal(err)
	}
	return src, r
}

// A bounded SpliceTo moves exactly count bytes, splitting a buffered block
// when it must, and leaves the rest to the next reader.
func TestSpliceToMovesExactlyItsCount(t *testing.T) {
	src, r := newPulledReader(t, 64<<10)
	src.pushIn(bytes.Repeat([]byte("abcdefgh"), 1024), false)
	dst := newFakeSource(64<<10, 64<<10)
	dst.state = NewState(dst)
	w := dst.state.Writer(0)
	if n, err := r.SpliceTo(context.Background(), w, 1000); n != 1000 || err != nil {
		t.Fatalf("SpliceTo(Writer, 1000) = (%d, %v)", n, err)
	}
	out, err := os.CreateTemp(t.TempDir(), "spliced-*")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if n, err := r.SpliceTo(context.Background(), out, 2000); n != 2000 || err != nil {
		t.Fatalf("SpliceTo(file, 2000) = (%d, %v)", n, err)
	}
	all := bytes.Repeat([]byte("abcdefgh"), 1024)
	got, _ := os.ReadFile(out.Name())
	if !bytes.Equal(dst.out.Bytes(), all[:1000]) || !bytes.Equal(got, all[1000:3000]) {
		t.Fatal("the bounded transfers did not split the stream where they should")
	}
	if r.Buffered() != len(all)-3000 {
		t.Fatalf("the reader holds %d bytes, want the %d left", r.Buffered(), len(all)-3000)
	}
}

// A bounded SpliceTo cut short by the end of the stream fails as io.ReadFull
// does; an unbounded one reports the end as success.
func TestSpliceToEndsAsReadFullDoes(t *testing.T) {
	src, r := newPulledReader(t, 64<<10)
	src.pushIn([]byte("short"), true)
	var out bytes.Buffer
	if n, err := r.SpliceTo(context.Background(), &out, 100); n != 5 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("SpliceTo(100) of 5 bytes = (%d, %v), want (5, io.ErrUnexpectedEOF)", n, err)
	}
	if n, err := r.SpliceTo(context.Background(), &out, 100); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("SpliceTo at the end = (%d, %v), want (0, io.EOF)", n, err)
	}
	if n, err := r.SpliceTo(context.Background(), &out, -1); n != 0 || err != nil {
		t.Fatalf("unbounded SpliceTo at the end = (%d, %v), want (0, nil)", n, err)
	}
	if _, err := r.SpliceTo(context.Background(), shortWriter{}, -1); err != nil {
		t.Fatalf("SpliceTo with nothing left wrote anyway: %v", err)
	}
}

// The last bytes a Pump hands over together with the end of the stream,
// between SpliceTo's drain of the pipe and its look at the end, still reach
// the destination before the end is reported.
func TestSpliceToDeliversTheBytesThatCameWithTheEnd(t *testing.T) {
	src, r := newPulledReader(t, 64<<10)
	src.pushIn([]byte("first "), false)
	pushed := false
	hook := func() {
		if !pushed {
			pushed = true
			src.pushIn([]byte("and last"), true)
		}
	}
	testHookPipeDrained.Store(&hook)
	defer testHookPipeDrained.Store(nil)
	var out bytes.Buffer
	n, err := r.SpliceTo(context.Background(), &out, -1)
	if n != 14 || err != nil || out.String() != "first and last" {
		t.Fatalf("SpliceTo = (%d, %v, %q), want all 14 bytes", n, err, out.String())
	}
}
