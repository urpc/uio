package uio

import (
	"sync"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/taskqueue"
)

type taskKind uint8

const (
	writeTask taskKind = iota
	flushTask
	closeTask
	wakeTask
	registerTask
	optionTask
	deadlineTask
	timeoutTask
	stopTask
)

type deadlineKind uint8

const (
	deadlineBoth deadlineKind = iota
	deadlineRead
	deadlineWrite
)

type socketOptionKind uint8

const (
	optionLinger socketOptionKind = iota
	optionNoDelay
	optionKeepAlive
	optionKeepAlivePeriod
	optionReadBuffer
	optionWriteBuffer
)

type task struct {
	node         taskqueue.Node[*task]
	kind         taskKind
	conn         *fdConn
	buf          *bytebuf.Buffer // owned by the task until runWriteTask transfers it
	err          error
	done         chan error
	registration *registerRequest

	deadline     time.Time
	deadlineKind deadlineKind
	generation   uint64
	optionKind   socketOptionKind
	optionValue  int
}

var taskPool = sync.Pool{New: func() any { return new(task) }}

func acquireTask(kind taskKind, conn *fdConn) *task {
	t := taskPool.Get().(*task)
	t.kind = kind
	t.conn = conn
	t.node.Value = t
	return t
}

func releaseTask(t *task) {
	// A non-nil buffer means task execution did not transfer ownership.
	if t.buf != nil {
		bytebuf.ReleaseBuffer(t.buf)
	}
	*t = task{}
	taskPool.Put(t)
}
