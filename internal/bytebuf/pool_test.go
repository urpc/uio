package bytebuf

import (
	"fmt"
	"testing"
)

func TestBufferPoolSizeClasses(t *testing.T) {
	for _, test := range []struct {
		requested int
		capacity  int
	}{
		{1, 512},
		{16, 512},
		{17, 512},
		{64, 512},
		{65, 512},
		{1024, 1024},
		{65536, 65536},
		{65537, 65537},
	} {
		buffer := getBuffer(test.requested)
		if got := buffer.Cap(); got != test.capacity {
			t.Errorf("getBuffer(%d) capacity = %d, want %d", test.requested, got, test.capacity)
		}
		putBuffer(buffer)
	}
}

func TestLargeOwnedSubClassAvoidsDoubleBloat(t *testing.T) {
	for _, tc := range []struct {
		req  int
		want int
	}{
		{64<<10 + 11, 72 << 10},    // 64KiB+11B -> 72KiB (instead of 128KiB)
		{128 << 10, 128 << 10},     // exact 128KiB -> 128KiB
		{128<<10 + 1, 144 << 10},   // 128KiB+1B -> 144KiB (instead of 256KiB)
		{256<<10 + 11, 288 << 10},  // 256KiB+11B -> 288KiB (instead of 512KiB)
		{1<<20 + 14, 1152 << 10},   // 1MiB+14B -> 1.125MiB (instead of 2MiB)
		{2<<20 + 11, 2304 << 10},   // 2MiB+11B -> 2.25MiB (instead of 4MiB)
		{4<<20 + 11, 4608 << 10},   // 4MiB+11B -> 4.5MiB (instead of 8MiB)
		{16 << 20, 16 << 20},       // exact 16MiB max pooled class
		{16<<20 + 17, 16<<20 + 17}, // > 16MiB allocates exact size and is not retained
	} {
		b := AcquireBuffer(tc.req)
		if got := b.Cap(); got != tc.want {
			t.Fatalf("AcquireBuffer(%d) cap = %d, want %d", tc.req, got, tc.want)
		}
		if tc.req <= maxOwnedBlock && b.Cap()%8192 != 0 {
			t.Fatalf("AcquireBuffer(%d) cap %d is not 8KiB page-aligned", tc.req, b.Cap())
		}
		ReleaseBuffer(b)

		// Also verify getBlock (used by CompositeBuffer.Gather / ReserveInbound).
		blk := getBlock(tc.req)
		if got := blk.Cap(); got != tc.want {
			t.Fatalf("getBlock(%d) cap = %d, want %d", tc.req, got, tc.want)
		}
		ReleaseBuffer(blk)
	}

	// Non-class grown buffer is safely ignored on ReleaseBuffer without corrupting buckets.
	odd := NewBuffer(make([]byte, 0, 100000))
	odd.poolKind = ownedBufferPool
	ReleaseBuffer(odd)
}

var poolSink int

// BenchmarkAcquireRelease borrows a block the way a write batch does: acquire,
// fill a little, release.
func BenchmarkAcquireRelease(b *testing.B) {
	for _, size := range []int{1024, 8 << 10, 64 << 10, 1<<20 + 14, 4<<20 + 11} {
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				buffer := AcquireBuffer(size)
				buffer.AvailableBuffer()[:64][0] = 1
				buffer.CommitWrite(64)
				poolSink += buffer.Len()
				ReleaseBuffer(buffer)
			}
		})
	}
}

// BenchmarkCloneRelease copies a write into a pooled block and releases it
// after the send, as a Write from outside the connection's turn does.
func BenchmarkCloneRelease(b *testing.B) {
	for _, size := range []int{64, 1024} {
		payload := make([]byte, size)
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				buffer := CloneBuffer(payload)
				poolSink += buffer.Len()
				ReleaseBuffer(buffer)
			}
		})
	}
}
