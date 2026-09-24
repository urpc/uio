//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"testing"
	"time"
)

func TestIOAdmissionRaceWithShutdown(t *testing.T) {
	for range 1000 {
		loop := &eventLoop{ioIdle: make(chan struct{}), ioPool: &ioTaskPool{}}
		conn := &fdConn{commonConn: commonConn{events: &Events{}, loop: loop}}
		start := make(chan struct{})
		producerDone := make(chan struct{})
		stopDone := make(chan struct{})
		go func() {
			<-start
			if conn.noteIO(ioEventRead) {
				conn.scheduled.Store(false)
				loop.releaseIO()
			}
			close(producerDone)
		}()
		go func() {
			<-start
			loop.stopIO()
			close(stopDone)
		}()
		close(start)
		select {
		case <-producerDone:
		case <-time.After(time.Second):
			t.Fatal("I/O producer did not finish during shutdown")
		}
		select {
		case <-stopDone:
		case <-time.After(time.Second):
			t.Fatal("shutdown lost an in-flight I/O reservation")
		}
		if conn.scheduled.Load() {
			t.Fatal("shutdown left a connection scheduled without an owner")
		}
	}
}
