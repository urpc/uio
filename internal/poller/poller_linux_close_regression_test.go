//go:build linux && !stdio

package poller

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Inject an empty probe while the descriptor registered with the runtime
// receives the real close wake. This models epoll_wait(..., 0) seeing the
// transient empty ready list of a concurrent epoll scan, without depending
// on hitting that kernel timing window in the test.
func TestParkedCloseAfterEmptyProbe(t *testing.T) {
	for _, closeDuringProbe := range []bool{false, true} {
		name := "after-probe"
		if closeDuringProbe {
			name = "during-probe"
		}
		t.Run(name, func(t *testing.T) {
			p, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close(nil)
			empty, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(empty)
			batch := &Batch{}
			if ok, err := p.beginWait(batch); !ok {
				t.Fatalf("beginWait: %v", err)
			}
			p.mountPark(batch)
			if batch.parkFile == nil {
				p.finishWait()
				t.Fatal("park was not mounted")
			}
			file := batch.parkFile
			// Only the callback's probe uses this empty epoll. The duplicate
			// watched by the runtime still refers to p.epfd and gets raised.
			batch.parkEpfd = empty
			read := batch.parkRead
			entered, resume := make(chan struct{}), make(chan struct{})
			first := true
			batch.parkRead = func(fd uintptr) bool {
				if first {
					first = false
					if closeDuringProbe {
						close(entered)
						<-resume
						return read(fd)
					}
					ready := read(fd)
					close(entered)
					<-resume
					return ready
				}
				return read(fd)
			}
			returned := make(chan error, 1)
			go func() {
				_, err := p.wait(batch, make([]Event, 8), -1, true)
				p.finishWait()
				returned <- err
			}()
			<-entered
			reason := errors.New("test close")
			_ = p.Close(reason)
			close(resume)
			select {
			case err := <-returned:
				if !errors.Is(err, reason) {
					t.Fatalf("wait error = %v, want %v", err, reason)
				}
			case <-time.After(2 * time.Second):
				// Cancel RawConn.Read so a failing baseline leaves no parked
				// goroutine or live descriptor behind.
				_ = file.Close()
				<-returned
				t.Fatal("Close left the waiter parked after an empty probe")
			}
		})
	}
}
