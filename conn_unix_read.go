//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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

package uio

import (
	"sync/atomic"

	"github.com/urpc/uio/internal/socket"
)

// testHookSocketClosePublished, when set, runs as a socket's close has been
// published and before it starts: tests hold it there.
var testHookSocketClosePublished atomic.Pointer[func(*fdConn)]

func (conn *fdConn) fireOnOpen() {
	if !conn.internal && conn.loop != nil && !conn.statOpened {
		conn.statOpened = true
		conn.loop.stats.noteConnOpened()
	}
	if callback := conn.callbacks().onOpen; callback != nil {
		started := conn.beginInboundCallback()
		callback(conn)
		conn.endInboundCallback(started)
	}
	if conn.taskState.Load()&taskScheduledBit == 0 {
		if err := conn.finishCallback(); err != nil {
			conn.requestClose(err)
		}
	}
}

// fireOnRead establishes the inbound-access scope around application code.
// Unread stream bytes remain available to a later callback; unread UDP bytes
// are discarded by the datagram caller because packet lifetime is one turn.
func (conn *fdConn) fireOnRead() error {
	var err error
	started := conn.beginInboundCallback()
	if w := conn.wake.Load(); w == nil {
		if callback := conn.callbacks().onRead; callback != nil {
			err = callback(conn)
		} else {
			_, _ = conn.Discard(-1)
		}
	} else {
		err = conn.fireOnReadControlled(w)
	}
	conn.endInboundCallback(started)
	if err != nil {
		return err
	}
	if conn.isClosing() {
		return nil
	}
	if conn.isDatagram() {
		return conn.finishCallback()
	}
	return nil
}

//go:noinline
func (conn *fdConn) fireOnReadControlled(w *wakeState) error {
	skipRead, err := conn.fireWakeTimer()
	if err != nil || skipRead || !conn.consumerMayRunControlled() {
		return err
	}
	if conn.entry.directConsumer && w.pullRead.Load() {
		conn.pumpBlockReader()
		return nil
	}
	if callback := conn.callbacks().onRead; callback != nil {
		return callback(conn)
	}
	_, _ = conn.Discard(-1)
	return nil
}

func (conn *fdConn) finishCallback() error {
	if conn.isClosing() {
		return nil
	}
	// The threshold batches within this callback; EAGAIN tails remain poller-driven.
	_, err := conn.flushOnLoop()
	return err
}

func (conn *fdConn) fireReadEvent() error {
	if conn.isDatagram() {
		return conn.onRecvUDP()
	}
	return conn.onRead()
}

func (conn *fdConn) fireWriteEvent() error {
	if conn.isDatagram() {
		return nil
	}
	conn.noteWritable()
	_, err := conn.flushOnLoop()
	return err
}

// ReserveInbound implements Conn.ReserveInbound.
func (conn *fdConn) ReserveInbound(n int) error {
	if conn.isDatagram() || n <= 0 {
		return nil
	}
	return conn.reserveInbound(n, conn.events().MaxInboundBuffered)
}

// onRead drains an edge-triggered stream with bounded work per task. The
// borrowed read buffer is exposed directly during OnRead; only a callback that
// leaves bytes unread pays the copy into the persistent inbound buffer.
func (conn *fdConn) onRead() error {
	if conn.ReadPaused() {
		return nil
	}
	if conn.flags.Load()&spliceReadFlag != 0 {
		if done, err := conn.readRoundSplice(conn.loadZeroCopy()); done || err != nil {
			return err
		}
	}
	holder := conn.events().readPool.Get()
	conn.scratch = holder
	// A read round is corked: its replies accumulate in scratch/outbound and are
	// flushed once when the event ends, or at the mid-round flush once they
	// pass midFlushThreshold, so a burst of reads costs one send instead of
	// one syscall per reply.
	conn.turn |= turnCorked
	err := conn.readRound(holder.bytes)
	conn.turn &^= turnCorked
	conn.scratch = nil
	// Unbind the borrowed tail before its holder returns to the pool: an
	// error exit leaves it aliasing the holder, and the close it asks for can
	// wait out a write in flight, long enough for another round to Get the
	// buffer back and overwrite it.
	conn.inboundTail = nil
	if out := holder.out; out != nil && out.Len() > 0 {
		if sendErr := conn.sendScratch(holder); err == nil {
			err = sendErr
		}
	}
	conn.events().readPool.Put(holder)
	return err
}

// readRound is onRead's body. It has many exits, which would keep deferred
// cleanup from being open-coded, so onRead does the cleanup around it.
func (conn *fdConn) readRound(buffer []byte) error {
	totalRead := 0
	for calls := 0; calls < 256 && totalRead < 1<<20; calls++ {
		n, err := socket.Recv(int(conn.fd), buffer)
		if err != nil {
			if isWouldBlock(err) {
				conn.turn = conn.turn&^turnReadToEnd | turnReadDrained
				return nil
			}
			conn.turn |= turnReadDrained
			return err
		}
		if n == 0 {
			conn.turn |= turnReadDrained
			return conn.onReadEOF()
		}
		totalRead += n
		// inboundTail aliases the pooled read buffer. Unconsumed bytes are copied
		// into inbound before the buffer is reused by the next syscall.
		conn.inboundTail = buffer[:n]
		if conn.callbacks().onBytesRead != nil {
			conn.events().onSocketBytesRead(conn, n)
		}
		if conn.isClosing() {
			conn.inboundTail = nil
			return nil
		}
		if err = conn.fireOnRead(); err != nil {
			return err
		}
		if conn.isClosing() {
			conn.inboundTail = nil
			return nil
		}
		if len(conn.inboundTail) > 0 {
			_, _ = conn.inbound.Write(conn.inboundTail)
			conn.inboundTail = nil
			if conn.holdsRead() {
				return nil
			}
		}
		if conn.ReadPaused() {
			return nil
		}
		if n < len(buffer) && conn.turn&(turnHangup|turnReadToEnd) == 0 {
			// The socket is drained, and bytes that arrive later raise a new
			// edge. A hangup already queued behind these bytes raises none,
			// which is why the round reads on once the poller reported one.
			return nil
		}
		// The round reads on. Once its replies fill four read buffers, send
		// them before reading further: a peer that keeps a window of requests
		// in flight is otherwise left waiting for the whole round, which can
		// run to 256 reads, and its pipeline empties. Smaller bursts still
		// leave in one write when the round ends. The threshold is four read
		// buffers rather than the coalescing block size: a mid-round send pays
		// a syscall and a writev of two blocks, and at one block of replies it
		// cost a pipeline 4-6% against waiting for the round's end.
		if conn.pending.Load() >= int64(conn.midFlushThreshold()) {
			if _, err := conn.flushOnLoop(); err != nil {
				return err
			}
		}
	}
	if totalRead > 0 && !conn.isClosing() && !conn.ReadPaused() && !conn.readHeld() {
		conn.taskState.Or(ioEventRead)
	}
	return nil
}

// udpReadBatch is the most datagrams one read round asks the kernel for; the
// round then runs one turn per datagram, in arrival order.
const udpReadBatch = 32

// grow doubles the batch after a full round, up to udpReadBatch slots.
func (batch *udpBatch) grow(slotSize int) {
	count := len(batch.slots) * 2
	if count > udpReadBatch {
		return
	}
	*batch = *newUDPBatch(slotSize, count)
}
