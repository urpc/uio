/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// The close walk: ends begun above or by a layer run through every layer on
// the connection's turn, CloseWrite waits for in-flight writers, and
// goodbyes flush before the socket closes.

package uio

import (
	"errors"
	"net"
)

// CloseWith closes the connection from above, as Conn.CloseWith: the close
// is requested and the call returns; every layer says its goodbye on the
// connection's next turn, the top one flushed first, so what it holds goes
// below ahead of the layers beneath saying theirs.
func (l *layerNode) CloseWith(err error) error {
	return l.endClose(err, gracefulCause(err), closeByConnWith)
}

// closeKind is who asked for a close, which decides what the request does
// when one is already under way.
type closeKind uint8

const (
	// closeByConnWith is Conn.CloseWith: on a connection a graceful drain
	// request already began closing, it ends the wait at once; on one a
	// CloseWith began, it yields — concurrent Close calls close once.
	closeByConnWith closeKind = iota
	// closeByConnGraceful is Conn.CloseGracefully: it drains, and yields to
	// a close already under way.
	closeByConnGraceful
	// closeByLayer is LayerContext.Close (graceful, drains) or Abort
	// (aborts, though: an abort ends a close in progress at once).
	closeByLayer
)

// endClose requests the connection's close, from above (Conn.CloseWith,
// Conn.CloseGracefully) or from this layer through LayerContext.Close and
// Abort; graceful chooses the mode — the goodbyes are said either way, and a
// graceful close also flushes each layer and drains the socket's queue.
//
// The request returns at once: the walk runs on the connection's turn, off
// every callback, so a close asked for inside one — a Handler's OnRead, a
// layer's OnWireSent, the writer's goroutine — never calls a layer back
// there, and a handler's CloseWith neither waits on a layer's goodbye nor
// shares its callback's stack. kind tells a close the layers above asked
// for, which walks from above the top (every layer says its goodbye, the
// top one flushed first) from one a layer began at itself: that layer is
// neither flushed nor called back on the turn — it may hold a lock its
// flush would take.
//
// When a close is already under way the kind decides: an abort, and a
// CloseWith on a drain a graceful request began, end it at once — the
// socket closes in one-shot mode and a walk still pending is dropped with
// it; a CloseGracefully, a layer's Close, and a CloseWith on a close a
// CloseWith began yield to it.
func (l *layerNode) endClose(cause error, graceful bool, kind closeKind) error {
	c := l.stack
	fromAbove := kind != closeByLayer
	if c.closedStack() {
		return net.ErrClosed
	}
	drain := kind != closeByConnWith && graceful
	if !c.beginClosing(l.index, drain) {
		// A close is already under way. Nothing is marked on this layer
		// here: the request either cuts the close in (and the end moves to
		// the socket) or yields to it, and a flag left on a layer that
		// yielded would stop the owner's walk at it — see closeWalk.
		// An abort ends any close in progress; a CloseWith closes cutting a
		// drain, and — when its own cause aborts — anything. Only a
		// graceful-caused CloseWith on a close another CloseWith began
		// yields, which is what keeps concurrent Close calls closing once.
		cut := (kind == closeByConnWith && (!graceful || c.drainBegun())) ||
			(kind == closeByLayer && !graceful)
		if !cut {
			if kind == closeByConnWith {
				return net.ErrClosed // a repeated Close: the one running is it
			}
			return nil // the close in progress owns the end
		}
		// It ends here. Taking the plan decides who owns the walk: one still
		// pending is dropped — its goodbyes never start; one a turn already
		// took is that walk's, and endAtSocket publishes the ending, which
		// stops it at its next layer (closeWalk). The socket's close then
		// follows either way.
		c.closePlan.Swap(nil)
		return c.endAtSocket(cause, false, false)
	}
	// This request owns the close: record its cause, mark the layer as walked
	// so a second request at it yields, and let the turn walk.
	c.recordCloseCause(cause)
	l.closing.Store(true)
	from := int32(l.index)
	if fromAbove {
		from = int32(len(c.layers))
	}
	c.closePlan.Store(&closePlan{cause: cause, graceful: graceful, from: from})
	c.requestTurn()
	return nil
}

// runClosePlan walks a close a layer, or a Handler above, began, on the
// turn, once: every request — LayerContext.Close/Abort, Conn.CloseWith,
// Conn.CloseGracefully — leaves its walk here.
func (c *stack) runClosePlan() {
	plan := c.closePlan.Swap(nil)
	if plan == nil || c.noLayerCalls() {
		return
	}
	c.recordCloseCause(plan.cause)
	switch {
	case plan.from == stackNoWriters:
		// The socket's own close, with no layer's goodbye between.
		_ = c.endAtSocket(plan.cause, c.beginClosing(stackNoWriters, false), plan.graceful)
	case int(plan.from) < len(c.layers):
		// The layer's own goodbye says first; it is not flushed — it may
		// hold a lock its flush would take — it sends what it holds itself.
		layer := c.layers[plan.from]
		_ = layer.closeWalk(plan.cause, plan.graceful, false)
	default:
		// Above the top: every layer says its goodbye, then the socket.
		if len(c.layers) == 0 {
			_ = c.endAtSocket(plan.cause, c.beginClosing(stackNoWriters, false), plan.graceful)
			return
		}
		top := c.layers[len(c.layers)-1]
		top.closing.Store(true)
		_ = top.closeWalk(plan.cause, plan.graceful, true)
	}
}

// closeWalk says this layer's goodbye and passes the close to the layer
// below, one layer at a time, until it reaches the socket. The stack is
// closing throughout — no turn calls a layer — and the walk is what still
// writes down: its goodbyes enter the layers below the initiator, and
// nothing above them.
func (l *layerNode) closeWalk(cause error, graceful, flushSelf bool) error {
	c := l.stack
	if c.noLayerCalls() {
		// A cut ended the close after the walk was committed (the plan was
		// taken before it): the layers from here on say nothing.
		return net.ErrClosed
	}
	if l.ops.graceful != nil && l.openUp() {
		l.ops.graceful(layerCtx{l}, cause)
	}
	if flushSelf && graceful && l.ops.flush != nil && l.opened.Load() && !c.noLayerCalls() {
		if err := l.ops.flush(layerCtx{l}); err != nil {
			// A layer's flush failed: the close aborts — the layers below are
			// not told to say their goodbyes, and the socket closes with no
			// drain.
			return c.endAtSocket(err, false, false)
		}
	}
	below, ok := l.lower.(*layerNode)
	if !ok {
		if at, ok := l.lower.(socketLower); ok {
			return at.socketClose(cause, graceful) // the socket, in the walk's mode
		}
		return l.lower.CloseWith(cause)
	}
	if !below.closing.CompareAndSwap(false, true) {
		return net.ErrClosed // a close from below owns that layer
	}
	return below.closeWalk(cause, graceful, true)
}

// CloseGracefully closes the connection gracefully whatever its cause: the
// close is requested and the call returns; the goodbyes are said on the
// connection's next turn, and the close waits for what was queued.
func (l *layerNode) CloseGracefully(cause error) error {
	return l.endClose(cause, true, closeByConnGraceful)
}

// CloseWrite ends the layer above's writing half, through the layer's
// protocol, after what was written before it: writes into the layer are
// refused from here on, what the layer holds is flushed first — a flush's
// error is CloseWrite's — and what it holds still, a send window's worth,
// goes below before the end does. A layer with no way to end one direction
// does not support it. Calling it again does nothing.
func (l *layerNode) CloseWrite() error {
	if l.ops.closeWriter == nil {
		return ErrUnsupported
	}
	if l.stack.over() {
		return net.ErrClosed
	}
	if !l.markWriteShut() {
		return nil
	}
	l.stack.shutsPending.Add(1)
	l.shutPending.Store(true)
	_, err := l.shutDown()
	return err
}

// shutDown passes the end CloseWrite asked of l below once what was written
// before it is: no write in flight into the layer, its output flushed, and
// nothing held. It reports false while something is, leaving the end to a
// turn's end, which the last write out or a flush asks for. One caller at a
// time claims the end.
func (l *layerNode) shutDown() (bool, error) {
	for l.writers.Load() == 0 && l.shutPending.CompareAndSwap(true, false) {
		if l.stack.over() {
			l.stack.shutsPending.Add(-1)
			return true, net.ErrClosed
		}
		ctx := layerCtx{l}
		if l.ops.flush != nil && !l.shutFlushed.Swap(true) {
			if err := l.ops.flush(ctx); err != nil {
				l.stack.shutsPending.Add(-1)
				return true, err
			}
			if l.stack.over() {
				// The flush closed the connection: the end has nothing left
				// to end, and the layer is not called again.
				l.stack.shutsPending.Add(-1)
				return true, net.ErrClosed
			}
		}
		if l.ops.held != nil && l.ops.held(ctx) > 0 {
			// Held bytes the layer cannot send yet: published again
			// before the look that may see them gone, so a report of
			// their leaving finds the end either way.
			l.shutPending.Store(true)
			if l.ops.held(ctx) > 0 {
				return false, nil
			}
			continue
		}
		l.stack.shutsPending.Add(-1)
		return true, l.ops.closeWriter(ctx)
	}
	return false, nil
}

// finishShuts passes down the ends CloseWrite left waiting whose layers no
// longer hold anything back. It runs as a turn ends; an error ends the
// connection through failTurn: nobody is left to return it to.
func (c *stack) finishShuts() {
	if c.shutsPending.Load() == 0 || c.closedStack() {
		return
	}
	for i := len(c.layers) - 1; i >= 0; i-- {
		l := c.layers[i]
		if !l.shutPending.Load() || c.over() {
			continue
		}
		if done, err := l.shutDown(); done && err != nil && !errors.Is(err, net.ErrClosed) {
			// A layer's end failed on the turn that passed it down: the
			// connection ends as any failed turn does — the goodbyes are
			// told the cause, and nothing reaches a layer again.
			_ = c.failTurn(err)
		}
	}
}
