package uio

import (
	"sync"

	"github.com/urpc/uio/internal/fdmap"
)

const (
	eventBatch              = 1024
	defaultTCPKeepAliveSecs = 15
	ioStopBit               = uint64(1 << 63)
)

var unixFdMap *fdmap.Map[fdConn]
var unixFdMapOnce sync.Once

func newFdMap() *fdmap.Map[fdConn] {
	// Unix can index directly by fd, so all loops share one sparse table.
	// Windows falls back to a typed, mutex-protected map per loop.
	if fdmap.UseSingleInstance {
		unixFdMapOnce.Do(func() { unixFdMap = fdmap.NewMap[fdConn]() })
		return unixFdMap
	}
	return fdmap.NewMap[fdConn]()
}

// cacheLineSize separates fields that every connection turn writes from the
// read-mostly fields next to them, so reading the latter does not keep
// missing on a line other CPUs just modified.
const cacheLineSize = 64
