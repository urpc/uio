//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"sync/atomic"
	"testing"
	"time"
)

type turnCountingExecutor struct{ tasks atomic.Int64 }

func (e *turnCountingExecutor) Submit(task IOTask) bool {
	e.tasks.Add(1)
	go task.RunTask()
	return true
}

func (e *turnCountingExecutor) SubmitBatch(tasks []IOTask) int {
	for _, task := range tasks {
		e.Submit(task)
	}
	return len(tasks)
}

// A Wake costs one turn, layered or not: no read is queued behind it unless
// a dropped read edge needs one.
func TestWakeCostsOneTurn(t *testing.T) {
	for _, layered := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "layered"}[layered], func(t *testing.T) {
			exec := &turnCountingExecutor{}
			reads := make(chan struct{}, 1)
			ev := &Events{Pollers: 1, Executor: exec, Handler: FuncHandler{Read: func(c Conn) error {
				_, _ = c.Discard(-1)
				select {
				case reads <- struct{}{}:
				default:
				}
				return nil
			}}}
			var layers []Layer
			if layered {
				layers = []Layer{PassThroughLayer}
			}
			_, server := readControlServe(t, ev, layers...)
			waitQuiet := func() {
				// Let the turn a callback ran in, and anything it queued, finish.
				for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
					before := exec.tasks.Load()
					time.Sleep(5 * time.Millisecond)
					if exec.tasks.Load() == before {
						return
					}
				}
			}
			select {
			case <-reads:
			case <-time.After(100 * time.Millisecond):
			}
			waitQuiet()
			const wakes = 50
			before := exec.tasks.Load()
			for range wakes {
				if err := server.Wake(); err != nil {
					t.Fatal(err)
				}
				readControlAwait(t, reads, "the wake's OnRead")
				waitQuiet()
			}
			if got := exec.tasks.Load() - before; got != wakes {
				t.Fatalf("%d wakes ran %d turns", wakes, got)
			}
		})
	}
}
