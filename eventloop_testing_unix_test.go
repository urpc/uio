//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import "sync"

// testLoopStops makes stopTestLoop idempotent per loop.
var testLoopStops sync.Map // *eventLoop -> *testLoopStop

type testLoopStop struct {
	once sync.Once
	done chan struct{}
}

// startTestLoop runs one waiter on loop, which a test publishes as the only
// loop of its Events, so a white-box test drives a loop the way Serve does.
func startTestLoop(events *Events, loop *eventLoop) {
	events.loops = []*eventLoop{loop}
	loop.start(1, false, false)
}

// stopTestLoop shuts loop down in the order Events does and returns a channel
// closed once it has. Later calls return the same channel.
func stopTestLoop(loop *eventLoop, err error) <-chan struct{} {
	value, _ := testLoopStops.LoadOrStore(loop, &testLoopStop{done: make(chan struct{})})
	stop := value.(*testLoopStop)
	stop.once.Do(func() {
		go func() {
			loop.stopping.Store(true)
			loop.closePoller(err)
			loop.shutdown(err)
			if loop.ioPool != nil && !loop.ioPoolOwner {
				loop.waitIODrained()
			}
			close(stop.done)
		}()
	})
	return stop.done
}
