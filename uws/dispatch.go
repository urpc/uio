package uws

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio"
)

const (
	// A runner yields by count or elapsed time so one hot or blocking
	// connection cannot retain an executor worker across an unbounded queue.
	maxDispatchEventsPerRun   = 64
	maxDispatchRunDuration    = time.Millisecond
	maxRetainedDispatchEvents = 64
	// The default global mailbox budget gives every shard at least one default
	// per-connection budget while spreading counters across cache lines.
	pendingBudgetShardCount = 64
)

type dispatchEventKind uint8

const (
	dispatchOpen dispatchEventKind = iota + 1
	dispatchMessage
	dispatchClose
)

type dispatchEvent struct {
	handshake   *handshakeState
	buffer      *uio.Buffer
	bytes       int
	kind        dispatchEventKind
	messageType MessageType
	budgetShard uint8
}

type dispatchPhase uint8

const (
	dispatchAccepting dispatchPhase = iota
	dispatchClosing
	// dispatchDone means the close event has been claimed by the runner. As in
	// the previous closeSent state, the callback may still be executing.
	dispatchDone
	dispatchRejected
)

type dispatchWriteFailure struct {
	err error
}

type dispatchWriteFailureState struct {
	pending        *dispatchWriteFailure
	blocksMessages bool
}

type dispatchState struct {
	executor   Executor
	limits     dispatchLimits
	budget     *pendingBudget
	mailbox    dispatchMailbox
	writeBatch dispatchWriteBatch
}

type dispatchLimits struct {
	maxMessages int
	maxBytes    int
}

type dispatchMailbox struct {
	// mu is never held while submitting executor work, invoking application
	// callbacks, acquiring the connection write lock, or closing the transport.
	mu              sync.Mutex
	queue           []dispatchEvent
	head            int
	runner          func()
	runnerActive    bool
	phase           dispatchPhase
	budgetStart     uint8
	writeFailure    dispatchWriteFailureState
	closeEvent      CloseEvent
	pendingMessages int
	pendingBytes    int
}

func newDispatchState(executor Executor, maxMessages, maxBytes int, budget *pendingBudget) *dispatchState {
	if executor == nil {
		return nil
	}
	return &dispatchState{
		executor: executor,
		limits: dispatchLimits{
			maxMessages: maxMessages,
			maxBytes:    maxBytes,
		},
		budget: budget,
		mailbox: dispatchMailbox{
			budgetStart: budget.nextShardIndex(),
		},
	}
}

type pendingBudget struct {
	next       atomic.Uint64
	shardCount uint64
	shards     [pendingBudgetShardCount]pendingBudgetShard
}

// Each shard occupies its own cache line. Connections prefer shards round-robin
// and only scan the others when their preferred shard is full.
type pendingBudgetShard struct {
	maxMessages int64
	maxBytes    int64
	messages    atomic.Int64
	bytes       atomic.Int64
	_           [32]byte
}

func (m *dispatchMailbox) appendEvent(event dispatchEvent) {
	if m.head >= 64 && m.head*2 >= len(m.queue) {
		copy(m.queue, m.queue[m.head:])
		m.queue = m.queue[:len(m.queue)-m.head]
		m.head = 0
	}
	m.queue = append(m.queue, event)
}

func (b *pendingBudget) configure(maxMessages, maxBytes int64) {
	shards := int64(pendingBudgetShardCount)
	if maxMessages > 0 {
		shards = min(shards, max(int64(1), maxMessages/int64(defaultMaxPendingMessages)))
	}
	if maxBytes > 0 {
		shards = min(shards, max(int64(1), maxBytes/int64(defaultMaxPendingBytes)))
	}
	shardCount := int(shards)
	b.shardCount = uint64(shardCount)
	b.next.Store(0)
	for index := range b.shards {
		shard := &b.shards[index]
		if index < shardCount {
			shard.maxMessages = shareBudget(maxMessages, index, shardCount)
			shard.maxBytes = shareBudget(maxBytes, index, shardCount)
		} else {
			shard.maxMessages = 0
			shard.maxBytes = 0
		}
		shard.messages.Store(0)
		shard.bytes.Store(0)
	}
}

func shareBudget(total int64, index, shards int) int64 {
	if total <= 0 {
		return total
	}
	share := total / int64(shards)
	if int64(index) < total%int64(shards) {
		share++
	}
	return share
}

func (b *pendingBudget) nextShardIndex() uint8 {
	if b == nil {
		return 0
	}
	shards := b.shardCount
	if shards == 0 {
		shards = 1
	}
	index := (b.next.Add(1) - 1) % shards
	return uint8(index)
}

func (b *pendingBudget) totals() (messages, bytes int64) {
	if b == nil {
		return 0, 0
	}
	for index := range b.shards {
		messages += b.shards[index].messages.Load()
		bytes += b.shards[index].bytes.Load()
	}
	return messages, bytes
}

func (b *pendingBudgetShard) reserve(size int) bool {
	if b == nil {
		return true
	}
	if b.maxMessages > 0 {
		for {
			current := b.messages.Load()
			if current >= b.maxMessages || !b.messages.CompareAndSwap(current, current+1) {
				if current >= b.maxMessages {
					return false
				}
				continue
			}
			break
		}
	}
	if b.maxBytes > 0 {
		for {
			current := b.bytes.Load()
			if int64(size) > b.maxBytes-current {
				if b.maxMessages > 0 {
					b.messages.Add(-1)
				}
				return false
			}
			if b.bytes.CompareAndSwap(current, current+int64(size)) {
				break
			}
		}
	}
	return true
}

func (b *pendingBudgetShard) release(size int) {
	if b == nil {
		return
	}
	if b.maxMessages > 0 {
		b.messages.Add(-1)
	}
	if b.maxBytes > 0 {
		b.bytes.Add(-int64(size))
	}
}

func (b *pendingBudget) reserve(start uint8, size int) (uint8, bool) {
	if b == nil {
		return 0, true
	}
	shardCount := int(b.shardCount)
	if shardCount == 0 {
		shardCount = 1
	}
	index := int(start)
	if index >= shardCount {
		index = 0
	}
	for remaining := shardCount; remaining > 0; remaining-- {
		if b.shards[index].reserve(size) {
			return uint8(index), true
		}
		index++
		if index == shardCount {
			index = 0
		}
	}
	return 0, false
}

func (b *pendingBudget) release(shard uint8, size int) {
	if b != nil {
		b.shards[shard].release(size)
	}
}

func (s *dispatchState) scheduleLocked(c *Conn) (func(), bool) {
	mailbox := &s.mailbox
	if mailbox.runner == nil {
		mailbox.runner = c.runDispatch
	}
	submit := !mailbox.runnerActive
	mailbox.runnerActive = true
	return mailbox.runner, submit
}

func (s *dispatchState) enqueueOpen(c *Conn, handshake *handshakeState) (func(), bool, error) {
	mailbox := &s.mailbox
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if mailbox.phase != dispatchAccepting {
		return nil, false, ErrClosed
	}
	mailbox.appendEvent(dispatchEvent{kind: dispatchOpen, handshake: handshake})
	runner, submit := s.scheduleLocked(c)
	return runner, submit, nil
}

func (s *dispatchState) checkMessageLocked(c *Conn, size, maxMessages, maxBytes int) error {
	mailbox := &s.mailbox
	if mailbox.phase != dispatchAccepting || mailbox.writeFailure.blocksMessages ||
		c.closed.Load() || c.closing.Load() {
		return ErrClosed
	}
	if mailbox.pendingMessages >= maxMessages || size > maxBytes-mailbox.pendingBytes {
		return ErrApplicationBackpressure
	}
	return nil
}

func (s *dispatchState) preflightMessage(c *Conn, size, maxMessages, maxBytes int) error {
	mailbox := &s.mailbox
	mailbox.mu.Lock()
	err := s.checkMessageLocked(c, size, maxMessages, maxBytes)
	mailbox.mu.Unlock()
	return err
}

func (s *dispatchState) enqueueMessageEvent(
	c *Conn,
	event dispatchEvent,
	maxMessages, maxBytes int,
) (func(), bool, error) {
	mailbox := &s.mailbox
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if err := s.checkMessageLocked(c, event.bytes, maxMessages, maxBytes); err != nil {
		return nil, false, err
	}
	mailbox.appendEvent(event)
	mailbox.pendingMessages++
	mailbox.pendingBytes += event.bytes
	runner, submit := s.scheduleLocked(c)
	return runner, submit, nil
}

func (s *dispatchState) enqueueClose(c *Conn, info CloseEvent) (func(), bool) {
	mailbox := &s.mailbox
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if mailbox.phase != dispatchAccepting {
		return nil, false
	}
	mailbox.phase = dispatchClosing
	oldQueue := mailbox.queue[mailbox.head:]
	kept := make([]dispatchEvent, 0, len(oldQueue))
	for index, event := range oldQueue {
		switch event.kind {
		case dispatchOpen:
			kept = append(kept, event)
		case dispatchMessage:
			s.budget.release(event.budgetShard, event.bytes)
			uio.ReleaseBuffer(event.buffer)
		}
		oldQueue[index] = dispatchEvent{}
	}
	mailbox.queue = kept
	mailbox.head = 0
	mailbox.closeEvent = info
	mailbox.appendEvent(dispatchEvent{kind: dispatchClose})
	mailbox.pendingMessages = 0
	mailbox.pendingBytes = 0
	return s.scheduleLocked(c)
}

func (s *dispatchState) reject(c *Conn) bool {
	mailbox := &s.mailbox
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if mailbox.phase == dispatchRejected {
		return false
	}
	mailbox.phase = dispatchRejected
	mailbox.runnerActive = false
	mailbox.writeFailure = dispatchWriteFailureState{}
	for index := mailbox.head; index < len(mailbox.queue); index++ {
		switch mailbox.queue[index].kind {
		case dispatchOpen:
			c.releaseHandshakeState(mailbox.queue[index].handshake)
		case dispatchMessage:
			s.budget.release(mailbox.queue[index].budgetShard, mailbox.queue[index].bytes)
			uio.ReleaseBuffer(mailbox.queue[index].buffer)
		}
		mailbox.queue[index] = dispatchEvent{}
	}
	mailbox.queue = nil
	mailbox.head = 0
	mailbox.pendingMessages = 0
	mailbox.pendingBytes = 0
	return true
}

type dispatchNext struct {
	event        dispatchEvent
	closeEvent   CloseEvent
	ok           bool
	writeFailure bool
}

func (s *dispatchState) nextEvent() dispatchNext {
	mailbox := &s.mailbox
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if mailbox.writeFailure.pending != nil {
		return dispatchNext{writeFailure: true}
	}
	if mailbox.head >= len(mailbox.queue) {
		return dispatchNext{}
	}
	event := mailbox.queue[mailbox.head]
	mailbox.queue[mailbox.head] = dispatchEvent{}
	mailbox.head++
	result := dispatchNext{event: event, ok: true}
	switch event.kind {
	case dispatchMessage:
		mailbox.pendingMessages--
		mailbox.pendingBytes -= event.bytes
		s.budget.release(event.budgetShard, event.bytes)
	case dispatchClose:
		mailbox.phase = dispatchDone
		result.closeEvent = mailbox.closeEvent
	}
	return result
}

type dispatchRunAction uint8

const (
	dispatchRunIdle dispatchRunAction = iota
	dispatchRunResubmit
	dispatchRunWriteFailure
)

func (s *dispatchState) finishRun() (dispatchRunAction, func()) {
	mailbox := &s.mailbox
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if mailbox.writeFailure.pending != nil {
		return dispatchRunWriteFailure, nil
	}
	if mailbox.head >= len(mailbox.queue) {
		if cap(mailbox.queue) > maxRetainedDispatchEvents {
			mailbox.queue = nil
		} else {
			mailbox.queue = mailbox.queue[:0]
		}
		mailbox.head = 0
		mailbox.runnerActive = false
		return dispatchRunIdle, nil
	}
	return dispatchRunResubmit, mailbox.runner
}

func (s *dispatchState) recordWriteFailure(err error) bool {
	mailbox := &s.mailbox
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if mailbox.writeFailure.pending != nil {
		return false
	}
	mailbox.writeFailure.blocksMessages = true
	mailbox.writeFailure.pending = &dispatchWriteFailure{err: err}
	return true
}

func (s *dispatchState) consumeWriteFailure() (func(), bool, bool) {
	mailbox := &s.mailbox
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if mailbox.writeFailure.pending == nil {
		return nil, false, false
	}
	pending := mailbox.writeFailure.pending
	mailbox.writeFailure.pending = nil
	closeQueued := false
	for index := mailbox.head; index < len(mailbox.queue); index++ {
		if mailbox.queue[index].kind == dispatchClose {
			mailbox.closeEvent.Err = errors.Join(mailbox.closeEvent.Err, pending.err)
			closeQueued = true
			break
		}
	}
	restart := closeQueued && mailbox.phase != dispatchRejected
	mailbox.runnerActive = restart
	return mailbox.runner, restart, true
}

func (c *Conn) dispatchOpen() error {
	handshake := c.handshake.Load()
	handler := c.callbackHandler()
	if handler == nil {
		c.releaseHandshakeState(handshake)
		return nil
	}
	state := c.dispatch
	if state == nil {
		defer c.releaseHandshakeState(handshake)
		handler.OnOpen(c)
		return nil
	}
	runner, submit, err := state.enqueueOpen(c, handshake)
	if err != nil {
		c.releaseHandshakeState(handshake)
		return err
	}
	if submit && !state.executor.Submit(runner) {
		c.failDispatch(ErrExecutorRejected)
		return ErrExecutorRejected
	}
	return nil
}

func (c *Conn) enqueueMessage(message Message) error {
	handler := c.callbackHandler()
	if handler == nil {
		return nil
	}
	if c.closed.Load() || c.closing.Load() {
		return ErrClosed
	}
	state := c.dispatch
	if state == nil {
		handler.OnMessage(c, message)
		return nil
	}

	maxMessages := state.limits.maxMessages
	if maxMessages <= 0 {
		maxMessages = defaultMaxPendingMessages
	}
	maxBytes := state.limits.maxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxPendingBytes
	}
	if err := state.preflightMessage(c, len(message.Payload), maxMessages, maxBytes); err != nil {
		return err
	}
	budgetShard, reserved := state.budget.reserve(state.mailbox.budgetStart, len(message.Payload))
	if !reserved {
		return ErrApplicationBackpressure
	}

	// The parser owns message.Payload until this callback returns. Transfer a
	// pooled copy to the queued event so an executor may run later without a
	// per-message heap allocation after the pool is warm.
	owned := uio.AcquireBuffer(len(message.Payload))
	_, _ = owned.Write(message.Payload)
	payloadBytes := owned.Len()
	runner, submit, err := state.enqueueMessageEvent(c, dispatchEvent{
		kind:        dispatchMessage,
		messageType: message.Type,
		buffer:      owned,
		bytes:       payloadBytes,
		budgetShard: budgetShard,
	}, maxMessages, maxBytes)
	if err != nil {
		state.budget.release(budgetShard, payloadBytes)
		uio.ReleaseBuffer(owned)
		return err
	}
	if submit && !state.executor.Submit(runner) {
		c.failDispatch(ErrExecutorRejected)
		return ErrExecutorRejected
	}
	return nil
}

func (c *Conn) dispatchClose(info CloseEvent) {
	handler := c.callbackHandler()
	if handler == nil {
		return
	}
	state := c.dispatch
	if state == nil {
		handler.OnClose(c, info)
		return
	}
	runner, submit := state.enqueueClose(c, info)
	if submit && !state.executor.Submit(runner) {
		c.failDispatch(ErrExecutorRejected)
	}
}

func (c *Conn) failDispatch(cause error) {
	state := c.dispatch
	if state == nil {
		return
	}
	if !state.reject(c) {
		return
	}

	if c.raw != nil {
		c.setCloseError(cause)
		c.closing.Store(true)
		if err := c.closeTransport(); err != nil {
			_ = c.raw.CloseWith(errors.Join(cause, err))
		}
	}
}

func (c *Conn) runDispatch() {
	state := c.dispatch
	if state == nil {
		return
	}
	batchOpen, err := c.beginDispatchWrites()
	if err != nil {
		c.reportDispatchWriteError(err)
	}
	defer func() {
		if batchOpen {
			if err := c.endDispatchWrites(); err != nil {
				c.reportDispatchWriteError(err)
			}
		}
	}()

	started := time.Now()
	for processed := 0; processed < maxDispatchEventsPerRun; processed++ {
		next := state.nextEvent()
		if next.writeFailure {
			c.stopDispatchForWriteFailure(&batchOpen)
			return
		}
		if !next.ok {
			break
		}
		event := next.event
		closeEvent := next.closeEvent

		switch event.kind {
		case dispatchOpen:
			func() {
				defer c.releaseHandshakeState(event.handshake)
				c.callbackHandler().OnOpen(c)
			}()
		case dispatchMessage:
			func() {
				defer uio.ReleaseBuffer(event.buffer)
				c.callbackHandler().OnMessage(c, Message{Type: event.messageType, Payload: event.buffer.Bytes()})
			}()
		case dispatchClose:
			if err := c.flushDispatchBatch(); err != nil {
				closeEvent.Err = errors.Join(closeEvent.Err, err)
			}
			c.callbackHandler().OnClose(c, closeEvent)
		}
		if time.Since(started) >= maxDispatchRunDuration {
			break
		}
	}
	if batchOpen {
		batchOpen = false
		if err := c.endDispatchWrites(); err != nil {
			c.reportDispatchWriteError(err)
		}
	}
	action, runner := state.finishRun()
	if action == dispatchRunWriteFailure {
		c.stopDispatchForWriteFailure(&batchOpen)
		return
	}
	if action == dispatchRunIdle {
		return
	}
	if !state.executor.Submit(runner) {
		c.failDispatch(ErrExecutorRejected)
	}
}

func (c *Conn) reportDispatchWriteError(err error) {
	state := c.dispatch
	if state == nil || err == nil {
		return
	}
	if !state.recordWriteFailure(err) {
		return
	}
	c.setCloseError(err)
	c.closing.Store(true)
	if c.raw != nil && !c.closed.Load() {
		_ = c.raw.CloseWith(err)
	}
}

func (c *Conn) stopDispatchForWriteFailure(batchOpen *bool) bool {
	state := c.dispatch
	if state == nil {
		return false
	}
	if *batchOpen {
		*batchOpen = false
		if err := c.endDispatchWrites(); err != nil {
			c.reportDispatchWriteError(err)
		}
	}
	runner, restart, consumed := state.consumeWriteFailure()
	if !consumed {
		return false
	}
	if restart {
		if !state.executor.Submit(runner) {
			c.failDispatch(ErrExecutorRejected)
		}
	}
	return true
}

func (c *Conn) callbackHandler() Handler {
	return c.handler
}
