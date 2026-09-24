package uio

import (
	"sync"

	"github.com/petermattis/goid"
)

// Only long-lived event loops are registered. This is queried by rare
// synchronous APIs that would otherwise wait for another event loop; it is
// never updated around application callbacks or connection-task dispatch.
var activeEventLoops sync.Map

// currentGoroutineID recognizes the callback, connection task, or event loop
// that already owns an operation. It never provides synchronization: the
// callback mutex or connection scheduler must establish ownership first.
// Keep the runtime-dependent representation isolated here.
func currentGoroutineID() int64 {
	return goid.Get()
}

func isEventLoopGoroutine() bool {
	_, ok := activeEventLoops.Load(currentGoroutineID())
	return ok
}
