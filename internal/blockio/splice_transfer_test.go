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
	"os"
	"path/filepath"
	"testing"

	"github.com/urpc/uio/internal/bytebuf"
)

// fakeTransfer stands in for a source's splice engine: it drains what the
// Reader holds through the hook, then either moves bytes to the file itself or
// hands bytes back through Rescue and falls back to the copy. It exercises the
// Reader's side of the SpliceTransfer contract without a socket.
type fakeTransfer struct {
	*fakeSource
	move     []byte // the engine writes these to the file after the drain
	rescue   []byte // or hands these back and falls back
	during   []byte // bytes a turn pumps before the rescue, which must follow it
	rest     []byte // what the turns deliver after a rescue
	fallback bool
	drained  bool
}

func (f *fakeTransfer) TransferSplice(req SpliceRequest) (int64, bool, error) {
	total, _, _, err := req.Hooks.Drain(req.Total)
	f.drained = true
	if err != nil {
		return total, false, err
	}
	if f.rescue != nil {
		// A turn pumps while the transfer runs, so the pipe is not empty when
		// the rescue lands: the rescued bytes must go ahead of these.
		f.pushIn(f.during, false)
		buf := bytebuf.AcquireBuffer(len(f.rescue))
		_, _ = buf.Write(f.rescue)
		req.Hooks.Rescue(buf)
		f.pushIn(f.rest, true)
		return total, true, nil
	}
	if req.File != nil {
		if _, err := req.File.Write(f.move); err != nil {
			return total, false, err
		}
		total += int64(len(f.move))
	}
	return total, f.fallback, nil
}

// The Reader's buffered bytes reach the destination through Drain before the
// engine moves anything of its own.
func TestSpliceTransferDrainsTheReaderFirst(t *testing.T) {
	base := newFakeSource(64<<10, 64<<10)
	ft := &fakeTransfer{fakeSource: base, move: []byte("-moved-by-the-engine")}
	base.state = NewState(ft)
	r, err := base.state.Reader(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	base.pushIn([]byte("buffered-first"), false)
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	n, err := r.SpliceTo(context.Background(), out, -1)
	got, _ := os.ReadFile(out.Name())
	want := []byte("buffered-first-moved-by-the-engine")
	if err != nil || n != int64(len(want)) || !bytes.Equal(got, want) || !ft.drained {
		t.Fatalf("SpliceTo = (%d, %v), file %q, drained=%v; want %q", n, err, got, ft.drained, want)
	}
}

// Bytes the engine hands back through Rescue go to the head of the Reader's
// pipe, ahead of bytes a turn pumped while the transfer ran, and the copy
// carries on.
func TestSpliceTransferRescueReturnsToThePipeHead(t *testing.T) {
	base := newFakeSource(64<<10, 64<<10)
	ft := &fakeTransfer{fakeSource: base, rescue: []byte("-rescued"), during: []byte("-pumped-during"), rest: []byte("-rest"), fallback: true}
	base.state = NewState(ft)
	r, err := base.state.Reader(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	base.pushIn([]byte("buffered"), false)
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	n, err := r.SpliceTo(context.Background(), out, -1)
	got, _ := os.ReadFile(out.Name())
	want := []byte("buffered-rescued-pumped-during-rest")
	if err != nil || n != int64(len(want)) || !bytes.Equal(got, want) {
		t.Fatalf("SpliceTo = (%d, %v), file %q; want %q", n, err, got, want)
	}
}

// A source without the splice capability leaves SpliceTo to copy in user space.
func TestSpliceToWithoutTheCapabilityCopies(t *testing.T) {
	base := newFakeSource(64<<10, 64<<10)
	base.state = NewState(base)
	r, err := base.state.Reader(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	base.pushIn([]byte("copied-in-user-space"), true)
	var out bytes.Buffer
	n, err := r.SpliceTo(context.Background(), &out, -1)
	if err != nil || n != 20 || out.String() != "copied-in-user-space" {
		t.Fatalf("SpliceTo = (%d, %v, %q)", n, err, out.String())
	}
}
