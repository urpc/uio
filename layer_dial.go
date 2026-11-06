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

// Dialing through a chain: the probe attached to the connection's driver slot
// until the whole stack opened, so DialContext can return the wrapped
// connection — and refuse it with zero callbacks.

package uio

import (
	"context"
	"net"
	"sync/atomic"
)

func prepareLayeredDial(fdc *fdConn) func(context.Context) (Conn, error) {
	probe := &dialProbe{done: make(chan struct{})}
	fdc.setDriverData(probe)
	return func(ctx context.Context) (Conn, error) {
		return awaitDial(ctx, fdc, probe)
	}
}

// dialProbe is the marker a dial carries until its stack opens: the stack
// settles or fails it — or the dial gives up on it first. Whichever claims it
// first decides.
type dialProbe struct {
	claimed atomic.Bool
	done    chan struct{} // closed once, with conn or err in place
	conn    Conn
	err     error
}

// settle reports the opened stack, unless the dial gave up already.
func (p *dialProbe) settle(conn Conn) bool {
	if !p.claimed.CompareAndSwap(false, true) {
		return false
	}
	p.conn = conn
	close(p.done)
	return true
}

func (p *dialProbe) fail(err error) {
	if err == nil {
		err = net.ErrClosed
	}
	if p.claimed.CompareAndSwap(false, true) {
		p.err = err
		close(p.done)
	}
}

// awaitDial waits for a dial's stack to open and reports what it became.
// raw is the socket, for a cancellation to close.
func awaitDial(ctx context.Context, raw Conn, probe *dialProbe) (Conn, error) {
	select {
	case <-probe.done:
		if probe.err != nil {
			return nil, probe.err
		}
		return probe.conn, nil
	case <-ctx.Done():
		cause := context.Cause(ctx)
		if cause == nil {
			cause = ctx.Err()
		}
		if !probe.claimed.CompareAndSwap(false, true) {
			<-probe.done // settled or failed as the deadline arrived
			if probe.err != nil {
				return nil, probe.err
			}
			return probe.conn, nil
		}
		_ = raw.Abort(cause)
		return nil, cause
	}
}
