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
// OnClosing frames are queued before the socket closes.

package uio

import (
	"errors"
	"net"
)

// endClose requests the connection's close, either from above (Conn.Close,
// Conn.Abort) or from this layer (LayerContext.Close, LayerContext.Abort).
// graceful selects draining vs immediate abort: OnClosing hooks run on the
// next turn either way, and a graceful close drains the socket's queue.
//
// When a close is already under way, a graceful request yields (returning
// net.ErrClosed for Conn.Close and nil for LayerContext.Close); an Abort cuts
// an ongoing graceful drain short at once, while yielding if an abort is
// already in progress.
func (l *layerNode) endClose(cause error, graceful, fromAbove bool) error {
	if fromAbove {
		l.stack.localClosed.Store(true)
	}
	if !graceful && cause == nil {
		cause = net.ErrClosed
	}
	c := l.stack
	if c.closedStack() {
		return net.ErrClosed
	}
	if !c.beginClosing(l.index, graceful) {
		if graceful {
			if fromAbove {
				return net.ErrClosed
			}
			return nil
		}
		// Abort while another close is under way: if an abort already claimed
		// the stack before reaching the socket, yield to it; otherwise cut the
		// graceful drain short at the socket.
		if c.state.Load()&stackPhaseMask == stackAborting {
			if fromAbove {
				return net.ErrClosed
			}
			return nil
		}
		if fromAbove {
			if st := c.block.Load(); st != nil {
				st.OnClose(true, cause)
			}
		}
		c.closePlan.Swap(nil)
		err := c.endAtSocket(cause, false, false)
		if !fromAbove && errors.Is(err, net.ErrClosed) && !c.closedStack() {
			return nil
		}
		return err
	}
	c.recordCloseCause(cause)
	if fromAbove {
		if st := c.block.Load(); st != nil {
			st.OnClose(true, cause)
		}
	}
	l.closing.Store(true)
	from := int32(l.index)
	if fromAbove {
		from = int32(len(c.layers))
	}
	c.closePlan.Store(&closePlan{cause: cause, graceful: graceful, from: from})
	c.requestTurn()
	return nil
}

// runClosePlan walks a close a layer or Handler began, on the turn, once.
func (c *stack) runClosePlan() {
	plan := c.closePlan.Swap(nil)
	if plan == nil || c.noLayerCalls() {
		return
	}
	c.recordCloseCause(plan.cause)
	if int(plan.from) < len(c.layers) {
		_ = c.layers[plan.from].closeWalk(plan.cause, plan.graceful)
		return
	}
	top := c.layers[len(c.layers)-1]
	top.closing.Store(true)
	_ = top.closeWalk(plan.cause, plan.graceful)
}

// closeWalk runs this layer's OnClosing and passes the close to the layer
// below, one layer at a time, until it reaches the socket.
func (l *layerNode) closeWalk(cause error, graceful bool) error {
	c := l.stack
	if c.noLayerCalls() {
		return net.ErrClosed
	}
	if l.ops.closing != nil && l.openUp() {
		l.ops.closing(layerCtx{l}, cause)
	}
	below, ok := l.lower.(*layerNode)
	if !ok {
		if at, ok := l.lower.(socketLower); ok {
			return at.socketClose(cause, graceful)
		}
		if graceful {
			return l.lower.Close()
		}
		return l.lower.Abort(cause)
	}
	if !below.closing.CompareAndSwap(false, true) {
		return net.ErrClosed // a close from below owns that layer
	}
	return below.closeWalk(cause, graceful)
}

// CloseWrite ends the layer above's writing half, through the layer's
// protocol, after what was written before it: writes into the layer are
// refused from here on, and any in-flight writes or held output go below
// before the end does. A layer with no way to end one direction does not
// support it. Calling it again does nothing.
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
	if ctrl := l.writeControl.Load(); ctrl != nil {
		ctrl.Notify(0)
	}
	l.stack.shutsPending.Add(1)
	l.shutPending.Store(true)
	done, err := l.shutDown()
	if done && err == nil && l.stack.halfOpen && l.stack.writeSideEnded(l.stack.layers[len(l.stack.layers)-1]) && l.stack.ended() {
		l.stack.requestTurn()
	}
	return err
}

// shutDown passes the end CloseWrite asked of l below once what was written
// before it is: no write in flight into the layer, and nothing held. It
// reports false while something is, leaving the end to a turn's end, which
// the last write out asks for. One caller at a time claims the end.
func (l *layerNode) shutDown() (bool, error) {
	for l.writers.Load() == 0 && l.shutPending.CompareAndSwap(true, false) {
		if l.stack.over() {
			l.stack.shutsPending.Add(-1)
			return true, net.ErrClosed
		}
		ctx := layerCtx{l}
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
		err := l.ops.closeWriter(ctx)
		l.stack.shutsPending.Add(-1)
		return true, err
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
		done, err := l.shutDown()
		if done && err != nil && !errors.Is(err, net.ErrClosed) {
			// A layer's end failed on the turn that passed it down: the
			// connection ends as any failed turn does — the goodbyes are
			// told the cause, and nothing reaches a layer again.
			_ = c.failTurn(err)
			return
		}
		if done && err == nil && c.halfOpen && c.writeSideEnded(c.layers[len(c.layers)-1]) && c.ended() {
			c.requestTurn()
		}
	}
}
