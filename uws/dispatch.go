package uws

import (
	"errors"
	"sync"
	"sync/atomic"
)

type dispatchEventKind uint8

const (
	dispatchOpen dispatchEventKind = iota + 1
	dispatchMessage
	dispatchClose
)

type dispatchEvent struct {
	kind    dispatchEventKind
	message Message
	close   CloseEvent
	bytes   int
}

type dispatchState struct {
	executor           Executor
	maxPendingMessages int
	maxPendingBytes    int
	budget             *pendingBudget
	mu                 sync.Mutex
	queue              []dispatchEvent
	running            bool
	closed             bool
	failed             bool
	closeSent          bool
	messages           int
	bytes              int
}

func newDispatchState(executor Executor, maxMessages, maxBytes int, budget *pendingBudget) *dispatchState {
	if executor == nil {
		return nil
	}
	return &dispatchState{
		executor:           executor,
		maxPendingMessages: maxMessages,
		maxPendingBytes:    maxBytes,
		budget:             budget,
	}
}

type pendingBudget struct {
	maxMessages int64
	maxBytes    int64
	messages    atomic.Int64
	bytes       atomic.Int64
}

func (b *pendingBudget) configure(maxMessages, maxBytes int64) {
	b.maxMessages = maxMessages
	b.maxBytes = maxBytes
	b.messages.Store(0)
	b.bytes.Store(0)
}

func (b *pendingBudget) reserve(size int) bool {
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

func (b *pendingBudget) release(size int) {
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

func (c *Conn) dispatchOpen() error {
	handler := c.callbackHandler()
	if handler == nil {
		return nil
	}
	state := c.dispatch
	if state == nil {
		handler.OnOpen(c)
		return nil
	}
	state.mu.Lock()
	if state.closed || state.failed {
		state.mu.Unlock()
		return ErrClosed
	}
	start := !state.running
	state.running = true
	state.queue = append(state.queue, dispatchEvent{kind: dispatchOpen})
	state.mu.Unlock()
	if start && !state.executor.Submit(c.runDispatch) {
		c.failDispatch()
		return ErrExecutorRejected
	}
	return nil
}

func (c *Conn) enqueueMessage(message Message) error {
	handler := c.callbackHandler()
	if handler == nil {
		return nil
	}
	state := c.dispatch
	if state == nil {
		handler.OnMessage(c, message)
		return nil
	}

	maxMessages := state.maxPendingMessages
	if maxMessages <= 0 {
		maxMessages = defaultMaxPendingMessages
	}
	maxBytes := state.maxPendingBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxPendingBytes
	}
	state.mu.Lock()
	if state.closed || state.failed || c.closed.Load() {
		state.mu.Unlock()
		return ErrClosed
	}
	if state.messages >= maxMessages || len(message.Payload) > maxBytes-state.bytes {
		state.mu.Unlock()
		return ErrApplicationBackpressure
	}
	state.mu.Unlock()
	if !state.budget.reserve(len(message.Payload)) {
		return ErrApplicationBackpressure
	}

	// The parser owns message.Payload until this callback returns. Copy it
	// before handing the event to an executor that may run later.
	payload := append([]byte(nil), message.Payload...)
	queued := Message{Type: message.Type, Payload: payload}
	state.mu.Lock()
	if state.closed || state.failed || c.closed.Load() {
		state.mu.Unlock()
		state.budget.release(len(payload))
		return ErrClosed
	}
	if state.messages >= maxMessages || len(payload) > maxBytes-state.bytes {
		state.mu.Unlock()
		state.budget.release(len(payload))
		return ErrApplicationBackpressure
	}
	start := !state.running
	state.running = true
	state.queue = append(state.queue, dispatchEvent{
		kind:    dispatchMessage,
		message: queued,
		bytes:   len(payload),
	})
	state.messages++
	state.bytes += len(payload)
	state.mu.Unlock()
	if start && !state.executor.Submit(c.runDispatch) {
		c.failDispatch()
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
	state.mu.Lock()
	if state.closeSent || state.closed {
		state.mu.Unlock()
		return
	}
	if state.failed {
		state.closeSent = true
		state.mu.Unlock()
		return
	}
	state.closed = true
	oldQueue := state.queue
	kept := make([]dispatchEvent, 0, len(oldQueue))
	for index, event := range oldQueue {
		if event.kind == dispatchOpen {
			kept = append(kept, event)
		} else if event.kind == dispatchMessage {
			state.budget.release(event.bytes)
		}
		oldQueue[index] = dispatchEvent{}
	}
	state.queue = append(kept, dispatchEvent{kind: dispatchClose, close: info})
	state.messages = 0
	state.bytes = 0
	start := !state.running
	state.running = true
	state.mu.Unlock()
	if start && !state.executor.Submit(c.runDispatch) {
		c.failDispatch()
	}
}

func (c *Conn) failDispatch() {
	state := c.dispatch
	if state == nil {
		return
	}
	state.mu.Lock()
	if state.failed {
		state.mu.Unlock()
		return
	}
	state.failed = true
	state.running = false
	for index := range state.queue {
		switch state.queue[index].kind {
		case dispatchMessage:
			state.budget.release(state.queue[index].bytes)
		case dispatchClose:
			state.closeSent = true
		}
		state.queue[index] = dispatchEvent{}
	}
	state.queue = nil
	state.messages = 0
	state.bytes = 0
	state.mu.Unlock()

	if c.raw != nil {
		c.setCloseError(ErrExecutorRejected)
		c.closing.Store(true)
		if err := c.closeTransport(); err != nil {
			_ = c.raw.CloseWith(errors.Join(ErrExecutorRejected, err))
		}
	}
}

func (c *Conn) runDispatch() {
	state := c.dispatch
	if state == nil {
		return
	}
	state.mu.Lock()
	if len(state.queue) == 0 {
		state.running = false
		state.mu.Unlock()
		return
	}
	event := state.queue[0]
	state.queue[0] = dispatchEvent{}
	state.queue = state.queue[1:]
	if event.kind == dispatchMessage {
		state.messages--
		state.bytes -= event.bytes
		state.budget.release(event.bytes)
	} else if event.kind == dispatchClose {
		state.closeSent = true
	}
	state.mu.Unlock()

	switch event.kind {
	case dispatchOpen:
		c.callbackHandler().OnOpen(c)
	case dispatchMessage:
		c.callbackHandler().OnMessage(c, event.message)
	case dispatchClose:
		c.callbackHandler().OnClose(c, event.close)
	}

	state.mu.Lock()
	if len(state.queue) == 0 {
		state.running = false
		state.mu.Unlock()
		return
	}
	state.mu.Unlock()
	if !state.executor.Submit(c.runDispatch) {
		c.failDispatch()
	}
}

func (c *Conn) callbackHandler() Handler {
	return c.handler
}
