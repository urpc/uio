package bytebuf

import (
	"bytes"
	"sync"
	"testing"
)

func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", name)
		}
	}()
	fn()
}

func TestSharedBufferImmutabilityIsolationAndMisuseGuards(t *testing.T) {
	// 1. Nil and empty handling.
	var nilSB *SharedBuffer
	if nilSB.Bytes() != nil || nilSB.Len() != 0 {
		t.Fatal("nil SharedBuffer returned non-empty")
	}
	nilSB.Release()
	assertPanics(t, "nil AcquireView", func() { _ = nilSB.AcquireView() })

	emptyA := FreezeBuffer(nil)
	emptyB := FreezeBuffer(AcquireBuffer(64))
	emptyC := NewSharedBuffer(nil)
	if emptyA.Len() != 0 || emptyB.Len() != 0 || emptyC.Len() != 0 {
		t.Fatal("empty SharedBuffer non-zero")
	}
	if v := emptyA.AcquireView(); v.Len() != 0 {
		t.Fatal("empty AcquireView non-empty")
	}
	emptyA.Release()
	emptyB.Release()
	emptyC.Release()

	// 2. Post-FreezeBuffer mutation or ReleaseBuffer on original *Buffer cannot touch frozen payload.
	orig := AcquireBuffer(128)
	_, _ = orig.WriteString("hello-broadcast")
	sb := FreezeBuffer(orig)
	if orig.Len() != 0 || orig.Cap() != 0 {
		t.Fatalf("FreezeBuffer did not clear original buffer: len=%d cap=%d", orig.Len(), orig.Cap())
	}
	_, _ = orig.WriteString("MUTATED-AFTER-FREEZE")
	ReleaseBuffer(orig)
	if string(sb.Bytes()) != "hello-broadcast" {
		t.Fatalf("post-freeze mutation leaked into SharedBuffer: %q", sb.Bytes())
	}

	// 3. NewSharedBuffer isolates caller's input slice.
	src := []byte("immutable-snapshot")
	sbCopy := NewSharedBuffer(src)
	src[0] = 'X'
	if string(sbCopy.Bytes()) != "immutable-snapshot" {
		t.Fatalf("caller slice mutation leaked into NewSharedBuffer: %q", sbCopy.Bytes())
	}
	sbCopy.Release()

	// 4. Independent view cursors, read-only mutation panics, and CompositeBuffer coalescing safety.
	v1 := sb.AcquireView()
	v2 := sb.AcquireView()
	if v1.Available() != 0 || cap(sb.Bytes()) != len(sb.Bytes()) {
		t.Fatalf("shared view exposed writable capacity: avail=%d cap=%d len=%d", v1.Available(), cap(sb.Bytes()), len(sb.Bytes()))
	}
	assertPanics(t, "Write on sharedView", func() { _, _ = v1.Write([]byte("x")) })
	assertPanics(t, "Truncate on sharedView", func() { v1.Truncate(2) })
	assertPanics(t, "CommitWrite on sharedView", func() { v1.CommitWrite(1) })

	// Partial read on v1 does not move v2's cursor.
	v1.Discard(6)
	if string(v1.Bytes()) != "broadcast" || string(v2.Bytes()) != "hello-broadcast" {
		t.Fatalf("cursor bleed: v1=%q v2=%q", v1.Bytes(), v2.Bytes())
	}
	// Draining v1 completely via Discard triggers Reset(), which must keep Available() == 0.
	v1.Discard(v1.Len())
	if v1.Available() != 0 {
		t.Fatalf("Reset on sharedView exposed Available() = %d, want 0", v1.Available())
	}
	ReleaseBuffer(v1)

	// Appending private data after v2 in CompositeBuffer must never overwrite sb.Bytes().
	var cb CompositeBuffer
	cb.AppendOwnedCoalesced(v2, 4096)
	_ = cb.WriteCoalesced([]byte("-private-tail"), 4096)
	copy(cb.Reserve(4, 4096), "-end")
	if string(sb.Bytes()) != "hello-broadcast" {
		t.Fatalf("CompositeBuffer coalescing corrupted SharedBuffer: %q", sb.Bytes())
	}
	got := make([]byte, cb.Len())
	_, _ = cb.Read(got)
	if string(got) != "hello-broadcast-private-tail-end" {
		t.Fatalf("CompositeBuffer stream = %q", got)
	}
	cb.Reset()

	// 5. Double Release() panics WITHOUT corrupting in-flight view refcount!
	inFlight := sb.AcquireView()
	sb.Release()
	assertPanics(t, "double Release", func() { sb.Release() })
	assertPanics(t, "AcquireView after Release", func() { _ = sb.AcquireView() })

	// Backing slice is STILL intact for the in-flight connection view until that view releases!
	if string(inFlight.Bytes()) != "hello-broadcast" {
		t.Fatalf("double Release corrupted in-flight view: %q", inFlight.Bytes())
	}
	ReleaseBuffer(inFlight)
}

func TestSharedViewConcurrentZeroAlloc(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 2048)
	sb := NewSharedBuffer(payload)
	defer sb.Release()

	// Warm pool once.
	ReleaseBuffer(sb.AcquireView())
	if allocs := testing.AllocsPerRun(1000, func() {
		v := sb.AcquireView()
		ReleaseBuffer(v)
	}); allocs != 0 {
		t.Fatalf("AcquireView+ReleaseBuffer allocs = %v, want 0", allocs)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				v := sb.AcquireView()
				if !bytes.Equal(v.Bytes(), payload) {
					t.Errorf("concurrent view payload mismatch")
				}
				v.Discard(128)
				ReleaseBuffer(v)
			}
		}()
	}
	wg.Wait()
}

func BenchmarkSharedViewAcquireRelease(b *testing.B) {
	sb := NewSharedBuffer(make([]byte, 4096))
	defer sb.Release()
	b.ReportAllocs()
	for range b.N {
		v := sb.AcquireView()
		ReleaseBuffer(v)
	}
}
