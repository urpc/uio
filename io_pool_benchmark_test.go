//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Run with:
//
//	go test -run '^$' -bench '^BenchmarkIOTaskSchedulers$' -benchmem -count=5
//
// Every scheduler executes the same runIOTask path through batch submission.
// The executor case puts a plain channel-fed worker set behind the Executor
// interface, for the cost of the injected path next to the owned scheduler.
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
			name: "UIODefault",
			new: func() ioBenchmarkScheduler {
				return &internalIOScheduler{pool: newIOTaskPool(nil)}
			},
		},
		{
			name: "UIOChannelExecutor",
			new: func() ioBenchmarkScheduler {
				executor := newChannelExecutor(workers)
				return &executorIOScheduler{pool: newIOTaskPool(executor), executor: executor}
			},
		},
	}
}

// channelExecutor is a minimal Executor: a fixed set of workers fed by one
// channel.
type channelExecutor struct {
	tasks   chan IOTask
	workers sync.WaitGroup
}

func newChannelExecutor(workers int) *channelExecutor {
	executor := &channelExecutor{tasks: make(chan IOTask, 64<<10)}
	for range workers {
		executor.workers.Add(1)
		go func() {
			defer executor.workers.Done()
			for task := range executor.tasks {
				task.RunTask()
			}
		}()
	}
	return executor
}

func (executor *channelExecutor) Submit(task IOTask) bool {
	executor.tasks <- task
	return true
}

func (executor *channelExecutor) SubmitBatch(tasks []IOTask) int {
	for _, task := range tasks {
		executor.tasks <- task
	}
	return len(tasks)
}

func (executor *channelExecutor) stop() {
	close(executor.tasks)
	executor.workers.Wait()
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

type executorIOScheduler struct {
	pool     *ioTaskPool
	executor *channelExecutor
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
	scheduler.executor.stop()
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
	events.OnData = func(Conn) error { runner.active.Load().Done(); return nil }
	runner.connections = make([]*fdConn, batch)
	runner.tasks = make([]IOTask, batch)
	for index := range runner.connections {
		// Distinct descriptors spread the turns over the scheduler's shards,
		// as real connections' do; no socket is touched.
		runner.connections[index] = &fdConn{commonConn: commonConn{events: events}, fd: int32(index)}
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
		// A wake turn runs a callback without touching a socket, which these
		// connections do not have.
		conn.taskState.Store(ioEventWake)
		conn.taskState.Or(taskScheduledBit)
		if !conn.loop.acquireIO() {
			panic("I/O benchmark loop stopped")
		}
	}
	if accepted := runner.scheduler.submitBatch(runner.tasks); accepted != len(runner.tasks) {
		panic("I/O task scheduler rejected benchmark task")
	}
	wait.Wait()
	for _, conn := range runner.connections {
		for conn.taskState.Load()&taskScheduledBit != 0 {
			runtime.Gosched()
		}
	}
	runner.active.Store(nil)
}
