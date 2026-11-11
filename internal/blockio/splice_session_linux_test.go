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

package blockio

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// fakeSpliceSource stands in for a native connection whose socket a SpliceTo
// takes over: the turn that grants it hands granted bytes to the Reader first,
// the socket then yields socket bytes into the pipe and ends, and once the
// socket is given back the turns pass the end of the stream on.
type fakeSpliceSource struct {
	*fakeSource
	granted, socket []byte
	grants, ends    int
	reads           int
}

func (f *fakeSpliceSource) RequestSplice() bool { return true }

func (f *fakeSpliceSource) SpliceGranted() (bool, bool) {
	if f.grants == 0 {
		// The granting round pumps what the connection held, after the
		// SpliceTo drained the Reader's pipe.
		f.pushIn(f.granted, false)
	}
	f.grants++
	return true, false
}

func (f *fakeSpliceSource) SpliceIn(pipeW, n int) (int, error) {
	f.reads++
	if f.reads > 1 {
		return 0, nil
	}
	return syscall.Write(pipeW, f.socket[:min(n, len(f.socket))])
}

func (f *fakeSpliceSource) EndSplice() {
	f.ends++
	f.pushIn(nil, true)
}

// fakeSpliceTarget is a Writer's destination for a session: it takes the
// pipe's bytes as a write of them.
func (f *fakeSource) SpliceFromPipe(pipeR, n int) (int, error) {
	buf := make([]byte, n)
	got := 0
	for got < n {
		r, err := syscall.Read(pipeR, buf[got:])
		if r > 0 {
			got += r
			continue
		}
		if err != nil {
			return got, err
		}
		break
	}
	if got == 0 {
		return 0, nil
	}
	return f.Write(buf[:got])
}

// The bytes the granting round hands the Reader leave before the bytes the
// splice takes from the socket after the grant, and the end of the stream the
// turns pass on ends the transfer.
func TestSpliceForwardsWhatTheGrantingRoundHandedOverFirst(t *testing.T) {
	base := newFakeSource(64<<10, 64<<10)
	src := &fakeSpliceSource{fakeSource: base, granted: []byte("handed over by the grant;"), socket: []byte("then spliced from the socket")}
	base.state = NewState(src)
	r, err := base.state.Reader(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	n, err := r.SpliceTo(context.Background(), out, -1)
	want := append(append([]byte(nil), src.granted...), src.socket...)
	got, _ := os.ReadFile(out.Name())
	if n != int64(len(want)) || err != nil || !bytes.Equal(got, want) {
		t.Fatalf("SpliceTo = (%d, %v), file %q; want %q", n, err, got, want)
	}
	if src.ends != 1 {
		t.Fatalf("the socket was given back %d times, want once", src.ends)
	}
}

// A SpliceTo cut short with bytes already in the kernel pipe hands them back
// to the Reader: the stream loses nothing, and reads on from them.
func TestSpliceToRescuesItsPipeWhenCutShort(t *testing.T) {
	base := newFakeSource(64<<10, 64<<10)
	payload := bytes.Repeat([]byte("spliced-bytes-"), 4096)
	src := &fakeSpliceSource{fakeSource: base, socket: payload}
	base.state = NewState(src)
	r, err := base.state.Reader(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	dst := newFakeSource(64<<10, 64<<10)
	dst.state = NewState(dst)
	w := dst.state.Writer(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	hook := func() { once.Do(cancel) }
	testHookSpliceFilled.Store(&hook)
	defer testHookSpliceFilled.Store(nil)
	n, err := r.SpliceTo(ctx, w, -1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SpliceTo = (%d, %v), want context.Canceled", n, err)
	}
	var rest bytes.Buffer
	if _, err := r.WriteTo(&rest); err != nil {
		t.Fatal(err)
	}
	got := append(append([]byte(nil), dst.out.Bytes()...), rest.Bytes()...)
	if !bytes.Equal(got, payload) || n+int64(rest.Len()) != int64(len(payload)) {
		t.Fatalf("the stream lost bytes: %d before the cut, %d of %d in all", n, len(got), len(payload))
	}
}
