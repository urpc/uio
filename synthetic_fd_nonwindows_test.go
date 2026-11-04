//go:build !windows

package uio

import (
	"sync/atomic"
	"testing"

	"github.com/urpc/uio/internal/fdmap"
)

// syntheticFD hands a made-up connection a descriptor the loop's fd table
// can hold. The table is sized from RLIMIT_NOFILE, which CI runners keep
// low, while the tests' hints sit far above it — a hint outside the table
// moves into its top quarter, one descriptor per call.
func syntheticFD(hint uintptr) uintptr {
	limit := uintptr(fdmap.MaxOpenFiles)
	if hint < limit/4 || limit < 128 {
		return hint
	}
	base := limit - limit/4
	return base + uintptr(syntheticFDSeq.Add(1))%(limit/4-1)
}

var syntheticFDSeq atomic.Uint64

// TestSyntheticDescriptorsFitTheFdTable pins the helper's bounds against a
// runner's small table.
func TestSyntheticDescriptorsFitTheFdTable(t *testing.T) {
	saved := fdmap.MaxOpenFiles
	fdmap.MaxOpenFiles = 256
	t.Cleanup(func() { fdmap.MaxOpenFiles = saved })

	table := fdmap.NewMap[fdConn]()
	seen := make(map[uintptr]bool)
	for i := range 16 {
		fd := syntheticFD(uintptr(30000 + i))
		if fd >= uintptr(fdmap.MaxOpenFiles) {
			t.Fatalf("descriptor %d outside the table of %d", fd, fdmap.MaxOpenFiles)
		}
		if seen[fd] {
			t.Fatalf("descriptor %d handed out twice", fd)
		}
		seen[fd] = true
		if err := table.Put(int(fd), nil); err != nil {
			t.Fatalf("descriptor %d refused: %v", fd, err)
		}
	}
	if fd := syntheticFD(0); fd != 0 {
		t.Fatalf("a hint inside the table was remapped to %d", fd)
	}
}
