//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"fmt"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func BenchmarkCallbackWrite(b *testing.B) {
	for _, size := range []int{64, 1024, 64 * 1024} {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer unix.Close(fds[1])
			if err = unix.SetNonblock(fds[0], true); err != nil {
				b.Fatal(err)
			}
			events := &Events{MaxPendingWrites: 1024}
			loop := &eventLoop{}
			loop.loopGoid.Store(currentGoroutineID())
			conn := &fdConn{fd: fds[0]}
			conn.events, conn.loop = events, loop
			payload := make([]byte, size)
			drainDone := make(chan struct{})
			drained := make(chan struct{})
			go func() {
				buffer := make([]byte, 64*1024)
				remaining := size
				for {
					n, readErr := unix.Read(fds[1], buffer)
					if readErr != nil || n == 0 {
						close(drainDone)
						return
					}
					remaining -= n
					if remaining == 0 {
						drained <- struct{}{}
						remaining = size
					}
				}
			}()

			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if _, err = conn.Write(payload); err != nil {
					b.Fatal(err)
				}
				for !conn.outbound.Empty() {
					conn.writeBlocked = false
					if _, err = conn.flushOnLoop(); err != nil {
						b.Fatal(err)
					}
					if !conn.outbound.Empty() {
						runtime.Gosched()
					}
				}
				<-drained
			}
			b.StopTimer()
			_ = unix.Close(fds[0])
			<-drainDone
		})
	}
}
