//go:build windows || stdio

package uio

import (
	"sync"
	"time"

	"github.com/urpc/uio/internal/taskqueue"
)

// taskKind identifies control-plane work that must run on an event loop.
type taskKind uint8

const (
	closeTask taskKind = iota
	wakeTask
	registerTask
	optionTask
	deadlineTask
	timeoutTask
	stopTask
	udpWriteTask
)

type udpWriteResult struct {
	n   int
	err error
}

// task is an intrusive MPSC queue item. Only the fields selected by kind are
// populated; releaseTask clears the full value before returning it to the pool.
type task struct {
	node         taskqueue.Node[*task]
	kind         taskKind
	conn         *fdConn
	err          error
	done         chan error
	registration *registerRequest
	acceptedTCP  bool // accepted TCP socket: apply the default socket options
	udpPayload   *Buffer
	udpDone      chan udpWriteResult

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
	*t = task{}
	taskPool.Put(t)
}
