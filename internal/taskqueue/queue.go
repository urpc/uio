// Package taskqueue provides a small intrusive MPSC FIFO queue.
package taskqueue

import "sync"

// Node is prepared by a producer before it enters the queue. Value is owned
// by the caller; the queue only links nodes.
type Node[T any] struct {
	Value T
	next  *Node[T]
}

// TakeNext detaches and returns the next node in a drained private batch.
func (n *Node[T]) TakeNext() *Node[T] {
	next := n.next
	n.next = nil
	return next
}

// Queue is a multi-producer, single-consumer FIFO. Its lock only protects
// acceptance state and pointer linkage.
type Queue[T any] struct {
	mu        sync.Mutex
	head      *Node[T]
	tail      *Node[T]
	accepting bool
}

// New returns an accepting queue.
func New[T any]() *Queue[T] {
	return &Queue[T]{accepting: true}
}

// Push appends node when the queue is still accepting work.
func (q *Queue[T]) Push(node *Node[T]) bool {
	q.mu.Lock()
	if !q.accepting {
		q.mu.Unlock()
		return false
	}
	q.pushLocked(node)
	q.mu.Unlock()
	return true
}

// Stop atomically stops accepting ordinary work and appends the final node.
// It returns false when another producer already stopped the queue.
func (q *Queue[T]) Stop(node *Node[T]) bool {
	q.mu.Lock()
	if !q.accepting {
		q.mu.Unlock()
		return false
	}
	q.accepting = false
	q.pushLocked(node)
	q.mu.Unlock()
	return true
}

func (q *Queue[T]) pushLocked(node *Node[T]) {
	if node == nil || node.next != nil {
		panic("taskqueue: invalid node")
	}
	if q.tail == nil {
		q.head = node
	} else {
		q.tail.next = node
	}
	q.tail = node
}

// Drain removes the complete public queue in O(1). The returned chain is
// private to the single consumer and remains in FIFO order.
func (q *Queue[T]) Drain() *Node[T] {
	q.mu.Lock()
	head := q.head
	q.head = nil
	q.tail = nil
	q.mu.Unlock()
	return head
}

// HasPending reports whether public work is waiting.
func (q *Queue[T]) HasPending() bool {
	q.mu.Lock()
	pending := q.head != nil
	q.mu.Unlock()
	return pending
}

// Accepting reports whether ordinary work may still be submitted.
func (q *Queue[T]) Accepting() bool {
	q.mu.Lock()
	accepting := q.accepting
	q.mu.Unlock()
	return accepting
}
