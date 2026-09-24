//go:build windows || stdio

package uio

// stdio keeps its dedicated blocking read/write goroutines. The native
// connection-task pool is intentionally absent on this backend.
type ioTaskPool struct{}

func newIOTaskPool(Executor) *ioTaskPool           { return nil }
func (pool *ioTaskPool) stop()                     {}
func (pool *ioTaskPool) submitBatch([]IOTask) bool { return false }
