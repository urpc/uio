package writecontrol

import "sync/atomic"

// Control notifies watchers when an output boundary retires wire bytes
// (sent > 0) or opens queue/window capacity without a socket send (sent == 0).
//
// Watchers run on the goroutine that retired the output or advanced the
// protocol window, so they must return promptly and must not block, write to,
// or close the connection synchronously.
type Control struct {
	watchers atomic.Pointer[[]func(int)]
	inexact  bool
}

// New constructs a write controller. inexact marks a boundary whose path
// passes through an OutboundHolder, where wire byte counts cannot prove a
// specific queued write left until OutboundBuffered reaches zero.
func New(inexact bool) *Control {
	return &Control{inexact: inexact}
}

// Inexact reports whether wire byte counts below this boundary may expand or
// retransmit held payload before reaching the socket.
func (c *Control) Inexact() bool {
	return c != nil && c.inexact
}

// Watch registers fn to receive future outbound progress notifications.
func (c *Control) Watch(fn func(sent int)) {
	if c == nil || fn == nil {
		return
	}
	for {
		old := c.watchers.Load()
		var next []func(int)
		if old == nil {
			next = []func(int){fn}
		} else {
			next = make([]func(int), len(*old)+1)
			copy(next, *old)
			next[len(*old)] = fn
		}
		if c.watchers.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Notify delivers an outbound progress or room event to every watcher.
func (c *Control) Notify(sent int) {
	if c == nil {
		return
	}
	if list := c.watchers.Load(); list != nil {
		for _, fn := range *list {
			fn(sent)
		}
	}
}
