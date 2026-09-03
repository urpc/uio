package uio

import "github.com/urpc/uio/internal/bytebuf"

// Buffer is reusable byte storage that can be transferred to a connection
// with [Conn.WriteOwned]. Newly acquired writable capacity is not zeroed; every
// committed byte must be initialized. A Buffer must not be used after its
// ownership is transferred or it is released.
type Buffer = bytebuf.Buffer

// AcquireBuffer returns an empty pooled buffer with at least capacity bytes of
// writable space. Release it when ownership is not transferred to WriteOwned.
func AcquireBuffer(capacity int) *Buffer {
	if capacity < 0 {
		panic("uio: negative buffer capacity")
	}
	return bytebuf.AcquireBuffer(capacity)
}

// ReleaseBuffer returns an unsubmitted buffer to the pool. It must be called at
// most once, and the buffer must not be used afterward.
func ReleaseBuffer(buffer *Buffer) { bytebuf.ReleaseBuffer(buffer) }
