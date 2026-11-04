//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"io"
	"testing"
)

// Terminal callbacks still belong to their reserved turns after transport
// resources are gone. Exercise both scheduler forms, including scratch reuse
// and a refused batch, and require every reservation and callback to finish.
func TestNativeSchedulerTerminalBatchOwnership(t *testing.T) {
	for _, mode := range []string{"typed-batch", "external-allocate", "external-reuse", "external-refused"} {
		t.Run(mode, func(t *testing.T) {
			var executor Executor
			if mode == "external-refused" {
				executor = rejectingNativeExecutor{}
			} else if mode != "typed-batch" {
				executor = nativeTaskExecutor{}
			}
			pool := newIOTaskPool(executor)
			t.Cleanup(pool.stop)
			loop := &eventLoop{ioPool: pool}
			finished := make(chan Conn, 3)
			cause := errors.New("transport already released")
			events := withFuncs(&Events{}, FuncHandler{Close: func(c Conn, err error) {
				if !errors.Is(err, cause) {
					t.Errorf("terminal close cause = %v", err)
				}
				finished <- c
			}})
			conns := make([]*fdConn, 3)
			tasks := make([]IOTask, len(conns))
			for i := range conns {
				c := &fdConn{fd: -1}
				c.loop, c.entry = loop, events.plainEntry()
				c.close.phase.Store(closeResourcesReleased)
				c.setDeferredCloseLocked(cause)
				c.taskState.Store(ioEventClose | taskScheduledBit)
				loop.acquireCloseIO()
				conns[i], tasks[i] = c, c
			}
			var args []IOTask
			if mode == "external-reuse" {
				args = make([]IOTask, len(conns)+2)
			}
			if mode == "typed-batch" {
				if !pool.submitBatch(tasks) {
					t.Fatal("owned scheduler refused terminal callbacks")
				}
			} else if !pool.submitConnBatch(conns, args) {
				t.Fatal("scheduler refused its completion handoff")
			}
			for _, item := range args {
				if item != nil {
					t.Fatal("caller scratch retained a connection after submission")
				}
			}
			seen := make(map[Conn]bool)
			for range conns {
				c := readControlAwait(t, finished, "terminal callback")
				if seen[c] {
					t.Fatal("terminal callback ran more than once")
				}
				seen[c] = true
			}
			loop.waitIODrained()
			pool.stop()
			if n := loop.ioTurnCount(); n != 0 {
				t.Fatalf("completion handoff retained %d turns", n)
			}
			for _, c := range conns {
				if c.close.phase.Load() != closeCallbackDelivered || c.taskState.Load()&taskScheduledBit != 0 {
					t.Fatal("terminal callback left its claim or close phase unfinished")
				}
			}
		})
	}
}

func TestNativeSchedulerStopRejectsFutureWorkAndFinishesLateRefusal(t *testing.T) {
	var absent *ioTaskPool
	absent.stop()
	if absent.submit(nil) || absent.submitWrite(nil) || absent.submitBatch(nil) || absent.submitConnBatch(nil, nil) {
		t.Fatal("absent scheduler accepted work")
	}
	// Abandoned tasks must also be harmless when an external executor invokes
	// one after registration failed before assigning a scheduler.
	(&fdConn{}).RunTask()
	(*writeTurn)(&fdConn{}).RunTask()
	(*writeTurn)(&fdConn{commonConn: commonConn{loop: &eventLoop{}}}).RunTask()

	for _, external := range []bool{false, true} {
		var executor Executor
		if external {
			executor = rejectingNativeExecutor{}
		}
		pool := newIOTaskPool(executor)
		if pool.submit(nil) || pool.submitWrite(nil) || !pool.submitBatch(nil) || !pool.submitConnBatch(nil, nil) {
			t.Fatal("empty submission changed scheduler ownership")
		}
		if external {
			opening := &fdConn{}
			opening.taskState.Store(ioEventOpen | taskScheduledBit)
			if pool.submit(opening) {
				t.Fatal("refused initial open was reported as accepted")
			}
		}
		pool.stop()
		unaccepted := &fdConn{}
		if pool.submit(unaccepted) || pool.submitWrite(unaccepted) ||
			pool.submitBatch([]IOTask{unaccepted}) || pool.submitConnBatch([]*fdConn{unaccepted}, nil) {
			t.Fatal("stopped scheduler accepted future work")
		}

		closed := make(chan error, 1)
		loop := &eventLoop{ioPool: pool}
		events := withFuncs(&Events{}, FuncHandler{Close: func(_ Conn, err error) { closed <- err }})
		conn := &fdConn{fd: -1}
		conn.loop, conn.entry = loop, events.plainEntry()
		conn.close.phase.Store(closeResourcesReleased)
		conn.setDeferredCloseLocked(io.EOF)
		conn.taskState.Store(ioEventClose | taskScheduledBit)
		loop.acquireCloseIO()
		// A batch can be refused just before stop and handed back afterwards.
		// Its completion must run inline once the failure queue is stopped.
		pool.rejectBatch([]IOTask{conn})
		if err := readControlAwait(t, closed, "late refused callback"); !errors.Is(err, io.EOF) {
			t.Fatalf("late refusal lost its terminal cause: %v", err)
		}
		if loop.ioTurnCount() != 0 || conn.close.phase.Load() != closeCallbackDelivered {
			t.Fatal("late refused callback retained its turn")
		}
	}
}

// A transport release can hand back its callback after the executor has gone
// away, or while an earlier turn still owns the scheduling claim. Both cases
// must finish exactly once without retaining the lifetime reservation.
func TestNativeTerminalCallbackSurvivesLostScheduler(t *testing.T) {
	for _, mode := range []string{"no-loop", "no-scheduler", "stopped-scheduler", "claimed-turn"} {
		t.Run(mode, func(t *testing.T) {
			closed := make(chan error, 2)
			events := withFuncs(&Events{}, FuncHandler{Close: func(_ Conn, err error) { closed <- err }})
			conn := &fdConn{fd: -1}
			conn.entry = events.plainEntry()
			conn.close.phase.Store(closeResourcesReleased)
			if mode != "no-loop" {
				conn.loop = &eventLoop{}
			}
			if mode == "stopped-scheduler" || mode == "claimed-turn" {
				pool := newIOTaskPool(nativeTaskExecutor{})
				t.Cleanup(pool.stop)
				conn.loop.ioPool = pool
				if mode == "stopped-scheduler" {
					pool.stop()
				} else {
					conn.taskState.Store(taskScheduledBit)
					conn.loop.acquireCloseIO()
				}
			}
			conn.scheduleCloseCallback(io.EOF)
			if mode == "claimed-turn" {
				select {
				case <-closed:
					t.Fatal("terminal delivery overtook the existing turn")
				default:
				}
				if !conn.loop.ioPool.submit(conn) {
					t.Fatal("executor refused its terminal turn")
				}
			}
			if err := readControlAwait(t, closed, "terminal callback after scheduler removal"); !errors.Is(err, io.EOF) {
				t.Fatalf("terminal cause=%v", err)
			}
			if conn.loop != nil {
				conn.loop.waitIODrained()
				if conn.loop.ioTurnCount() != 0 {
					t.Fatal("terminal fallback retained its reservation")
				}
			}
			// A competing completion may arrive after the phase was delivered.
			conn.fireCloseCallback()
			select {
			case <-closed:
				t.Fatal("terminal callback was delivered twice")
			default:
			}
		})
	}
}
