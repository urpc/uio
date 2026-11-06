//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"fmt"
	"golang.org/x/sys/unix"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
			events := &Events{}
			loop := &eventLoop{}
			conn := &fdConn{fd: int32(fds[0])}
			conn.loop = loop
			conn.entry = events.plainEntry()
			conn.ioOwner.Store(currentGoroutineID())
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
					conn.setWriteBlocked(false)
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

func BenchmarkIOTaskSchedulers(b *testing.B) {
	workers := runtime.GOMAXPROCS(0)
	for _, workload := range []struct {
		name  string
		batch int
	}{{name: "EventBatch", batch: eventBatch}, {name: "Burst", batch: workers}, {name: "OverflowStress", batch: 16 << 10}} {
		b.Run(workload.name, func(b *testing.B) {
			for _, factory := range ioSchedulerFactories(workers) {
				b.Run(factory.name, func(b *testing.B) {
					scheduler := factory.new()
					runner := newIOTaskBenchmarkRunner(workload.batch, scheduler)
					runner.runBatch()
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
	openKey()
	submitBatch([]*fdConn, []IOTask) int
	stop()
}

type ioBenchmarkFactory struct {
	name string
	new  func() ioBenchmarkScheduler
}

func ioSchedulerFactories(workers int) []ioBenchmarkFactory {
	return []ioBenchmarkFactory{{name: "UIODefault", new: func() ioBenchmarkScheduler {
		return &internalIOScheduler{pool: newIOTaskPool(nil)}
	}}, {name: "UIOChannelExecutor", new: func() ioBenchmarkScheduler {
		executor := newChannelExecutor(workers)
		return &executorIOScheduler{pool: newIOTaskPool(executor), executor: executor}
	}}}
}

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
	conn.loop = &eventLoop{ioPool: scheduler.pool, count: 1}
}

func (scheduler *internalIOScheduler) openKey() {
	scheduler.pool.connOpened()
}

func (scheduler *internalIOScheduler) submitBatch(conns []*fdConn, _ []IOTask) int {
	if scheduler.pool.submitConnBatch(conns, nil) {
		return len(conns)
	}
	return 0
}

func (scheduler *internalIOScheduler) stop() {
	scheduler.pool.stop()
}

type executorIOScheduler struct {
	pool     *ioTaskPool
	executor *channelExecutor
}

func (scheduler *executorIOScheduler) bind(conn *fdConn) {
	conn.loop = &eventLoop{ioPool: scheduler.pool, count: 1}
}

func (scheduler *executorIOScheduler) openKey() {
}

func (scheduler *executorIOScheduler) submitBatch(_ []*fdConn, tasks []IOTask) int {
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
	funcs(events).Read = func(Conn) error {
		runner.active.Load().Done()
		return nil
	}
	entry := events.plainEntry()
	runner.connections = make([]*fdConn, batch)
	runner.tasks = make([]IOTask, batch)
	for index := range runner.connections {
		runner.connections[index] = &fdConn{commonConn: commonConn{entry: entry}, fd: int32(index)}
		runner.tasks[index] = runner.connections[index]
		scheduler.bind(runner.connections[index])
		scheduler.openKey()
	}
	return runner
}

func (runner *ioTaskBenchmarkRunner) runBatch() {
	var wait sync.WaitGroup
	wait.Add(len(runner.connections))
	runner.active.Store(&wait)
	for _, conn := range runner.connections {
		conn.taskState.Store(ioEventWake)
		conn.taskState.Or(taskScheduledBit)
		if !conn.loop.acquireIO() {
			panic("I/O benchmark loop stopped")
		}
	}
	if accepted := runner.scheduler.submitBatch(runner.connections, runner.tasks); accepted != len(runner.tasks) {
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
