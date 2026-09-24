//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/limpo1989/taskgo"
)

// Run with:
//
//	go test -run '^$' -bench '^BenchmarkIOTaskSchedulers$' -benchmem -count=5
//
// Every scheduler executes the same runIOTask path through batch submission.
// Taskgo direct shows the queue's own cost; the UIO cases include lifecycle and
// rejection accounting.
func BenchmarkIOTaskSchedulers(b *testing.B) {
	workers := runtime.GOMAXPROCS(0)
	for _, workload := range []struct {
		name  string
		batch int
	}{
		{name: "Saturated", batch: 16 << 10},
		{name: "Burst", batch: workers},
	} {
		b.Run(workload.name, func(b *testing.B) {
			for _, factory := range ioSchedulerFactories(workers) {
				b.Run(factory.name, func(b *testing.B) {
					scheduler := factory.new()
					runner := newIOTaskBenchmarkRunner(workload.batch, scheduler)
					runner.runBatch() // warm workers and pooled callback state

					b.ReportAllocs()
					b.ResetTimer()
					started := time.Now()
					for range b.N {
						runner.runBatch()
					}
					elapsed := time.Since(started)
					b.StopTimer()
					tasks := int64(b.N) * int64(workload.batch)
					b.ReportMetric(float64(elapsed.Nanoseconds())/float64(tasks), "ns/task")
					b.ReportMetric(float64(tasks)/elapsed.Seconds(), "tasks/s")
					scheduler.stop()
				})
			}
		})
	}
}

type ioBenchmarkScheduler interface {
	bind(*fdConn)
	submitBatch([]IOTask) int
	stop()
}

type ioBenchmarkFactory struct {
	name string
	new  func() ioBenchmarkScheduler
}

func ioSchedulerFactories(workers int) []ioBenchmarkFactory {
	return []ioBenchmarkFactory{
		{
			name: "UIODefaultTaskgo",
			new: func() ioBenchmarkScheduler {
				return &internalIOScheduler{pool: newIOTaskPool(nil)}
			},
		},
		{
			name: "TaskgoDirect",
			new: func() ioBenchmarkScheduler {
				return &taskgoIOScheduler{queue: newTaskgoBenchmarkQueue(workers, func(task IOTask) {
					task.(*fdConn).runIOTask()
				})}
			},
		},
		{
			name: "UIOTaskgoExecutor",
			new: func() ioBenchmarkScheduler {
				queue := newTaskgoBenchmarkQueue(workers, func(task IOTask) { task.RunTask() })
				return &executorIOScheduler{pool: newIOTaskPool(queue), queue: queue}
			},
		},
	}
}

func newTaskgoBenchmarkQueue(workers int, run func(IOTask)) *taskgo.Task[IOTask] {
	return taskgo.NewTask(
		run,
		taskgo.WithConcurrency(workers),
		taskgo.WithMaxIdle(time.Minute),
	)
}

type internalIOScheduler struct{ pool *ioTaskPool }

func (scheduler *internalIOScheduler) bind(conn *fdConn) {
	conn.loop = &eventLoop{ioPool: scheduler.pool}
}
func (scheduler *internalIOScheduler) submitBatch(tasks []IOTask) int {
	if scheduler.pool.submitBatch(tasks) {
		return len(tasks)
	}
	return 0
}
func (scheduler *internalIOScheduler) stop() { scheduler.pool.stop() }

type taskgoIOScheduler struct{ queue *taskgo.Task[IOTask] }

func (scheduler *taskgoIOScheduler) bind(conn *fdConn) { conn.loop = &eventLoop{} }
func (scheduler *taskgoIOScheduler) submitBatch(tasks []IOTask) int {
	return scheduler.queue.SubmitBatch(tasks)
}
func (scheduler *taskgoIOScheduler) stop() { _ = scheduler.queue.Stop(context.Background()) }

type executorIOScheduler struct {
	pool  *ioTaskPool
	queue *taskgo.Task[IOTask]
}

func (scheduler *executorIOScheduler) bind(conn *fdConn) {
	conn.loop = &eventLoop{ioPool: scheduler.pool}
}
func (scheduler *executorIOScheduler) submitBatch(tasks []IOTask) int {
	if scheduler.pool.submitBatch(tasks) {
		return len(tasks)
	}
	return 0
}
func (scheduler *executorIOScheduler) stop() {
	scheduler.pool.stop()
	_ = scheduler.queue.Stop(context.Background())
}

type ioTaskBenchmarkRunner struct {
	connections []*fdConn
	tasks       []IOTask
	active      atomic.Pointer[sync.WaitGroup]
	scheduler   ioBenchmarkScheduler
}

func newIOTaskBenchmarkRunner(batch int, scheduler ioBenchmarkScheduler) *ioTaskBenchmarkRunner {
	runner := &ioTaskBenchmarkRunner{scheduler: scheduler}
	events := &Events{}
	events.OnOpen = func(Conn) { runner.active.Load().Done() }
	runner.connections = make([]*fdConn, batch)
	runner.tasks = make([]IOTask, batch)
	for index := range runner.connections {
		runner.connections[index] = &fdConn{commonConn: commonConn{events: events}}
		runner.tasks[index] = runner.connections[index]
		scheduler.bind(runner.connections[index])
	}
	return runner
}

func (runner *ioTaskBenchmarkRunner) runBatch() {
	var wait sync.WaitGroup
	wait.Add(len(runner.connections))
	runner.active.Store(&wait)
	for _, conn := range runner.connections {
		conn.pendingEvents.Store(ioEventOpen)
		conn.scheduled.Store(true)
		if !conn.loop.acquireIO() {
			panic("I/O benchmark loop stopped")
		}
	}
	if accepted := runner.scheduler.submitBatch(runner.tasks); accepted != len(runner.tasks) {
		panic("I/O task scheduler rejected benchmark task")
	}
	wait.Wait()
	for _, conn := range runner.connections {
		for conn.scheduled.Load() {
			runtime.Gosched()
		}
	}
	runner.active.Store(nil)
}
