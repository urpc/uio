//go:build windows || stdio

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

// The portable backend's outbound side: the write queue, its coalescing,
// owned buffers, batches and the write scratch.

package uio

import (
	"errors"
	"io"
	"net"
	"unsafe"

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/bytebuf"
)

func (fc *fdConn) WriteByte(b byte) error {
	var bb [1]byte
	bb[0] = b
	_, err := fc.Write(bb[:])
	return err
}

func (fc *fdConn) WriteString(s string) (n int, err error) {
	var data = unsafe.Slice(unsafe.StringData(s), len(s))
	return fc.Write(data)
}

// Write copies tiny payloads into the shared outbound tail and transfers larger
// payloads as owned blocks, keeping allocation and copying outside mux.
// coalesceBlockSize is the block size queued writes pack into while the peer
// is slow to drain: two read buffers' worth, capped at 64 KiB.
func (fc *fdConn) coalesceBlockSize() int {
	return min(64<<10, fc.events().readBufSize*2)
}

func (fc *fdConn) Write(p []byte) (n int, err error) {
	if fc.udp == nil && fc.entry != nil && fc.entry.directConsumer && fc.events().BlockingWrite {
		if fc.hasWaitingBlockWriter() && !blockio.OnTurn() {
			return fc.ensureBlockWriter().Write(p)
		}
		n, err = fc.writeNonBlocking(p)
		if errors.Is(err, ErrOutboundOverflow) && !blockio.OnTurn() {
			return fc.ensureBlockWriter().Write(p)
		}
		return n, err
	}
	return fc.writeNonBlocking(p)
}

func (fc *fdConn) writeNonBlocking(p []byte) (n int, err error) {
	if fc.isClosing() {
		return 0, net.ErrClosed
	}

	if fc.udp != nil {

		if nil == fc.udp.svr && nil == fc.udp.peers {
			// connected udp client.
			n, err = fc.udp.sock.Write(p)
		} else {
			// udp child connection.
			n, err = fc.udp.sock.WriteTo(p, net.UDPAddrFromAddrPort(fc.remoteAddr))
		}

		if n > 0 {
			fc.reportOutbound(n)
		}
		return
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(p) >= stdOwnedWriteThreshold {
		if limit := fc.payloadLimit(); limit > 0 && len(p) > limit {
			return 0, ErrOutboundOverflow
		}
		owned := bytebuf.CloneBuffer(p)
		n, err := fc.writeOwnedNonBlocking(owned)
		if errors.Is(err, ErrOutboundOverflow) {
			// The clone never entered the queue, so Write still owns it.
			bytebuf.ReleaseBuffer(owned)
		}
		return n, err
	}

	fc.mux.Lock()
	if fc.isClosing() || fc.draining.Load() || fc.writeShut {
		fc.mux.Unlock()
		return 0, net.ErrClosed
	}
	if limit := fc.payloadLimit(); limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-len(p) {
		fc.mux.Unlock()
		return 0, ErrOutboundOverflow
	}
	n = fc.outbound.WriteCoalesced(p, fc.coalesceBlockSize())
	fc.mux.Unlock()

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}

	return
}

// reportOutbound runs OnBytesWritten for a datagram send. Datagrams are sent on
// whichever goroutine writes them, so the calls for one connection go through
// a claim: a sender adds its bytes and reports them itself if it takes the
// claim, and otherwise leaves them to the holder, which reports until nothing
// is pending, so the calls never overlap and a send made inside OnBytesWritten is
// reported once that call returns. Nobody waits for the claim, so OnBytesWritten
// callbacks that write to each other cannot deadlock.
func (fc *fdConn) reportOutbound(n int) {
	if fc.callbacks().onBytesWritten == nil && !fc.hasWriteControl() {
		return
	}
	udp := fc.udp
	udp.outboundPending.Add(int64(n))
	for udp.outboundClaim.CompareAndSwap(false, true) {
		fc.drainOutboundReports()
		// Bytes added between the last drain and the release found the
		// claim taken; look again.
		if udp.outboundPending.Load() == 0 {
			return
		}
	}
}

// drainOutboundReports reports pending bytes while holding the claim, and
// releases it even when OnBytesWritten panics.
func (fc *fdConn) drainOutboundReports() {
	udp := fc.udp
	defer udp.outboundClaim.Store(false)
	for {
		pending := udp.outboundPending.Swap(0)
		if pending == 0 {
			return
		}
		fc.events().reportBytesWritten(fc, int(pending))
	}
}

// Writev preserves large source segments as separate owned blocks when the
// fixed writer vector can carry them; larger segment counts collapse into one
// block to avoid retaining an unbounded pointer list.
func (fc *fdConn) Writev(vec [][]byte) (n int, err error) {
	if fc.udp == nil && fc.entry != nil && fc.entry.directConsumer && fc.events().BlockingWrite {
		if fc.hasWaitingBlockWriter() && !blockio.OnTurn() {
			return fc.ensureBlockWriter().Writev(vec)
		}
		n, err = fc.writevNonBlocking(vec)
		if errors.Is(err, ErrOutboundOverflow) && !blockio.OnTurn() {
			return fc.ensureBlockWriter().Writev(vec)
		}
		return n, err
	}
	return fc.writevNonBlocking(vec)
}

func (fc *fdConn) writevNonBlocking(vec [][]byte) (n int, err error) {
	if fc.isClosing() {
		return 0, net.ErrClosed
	}

	if fc.udp != nil {
		// Each buffer is one datagram; the standard library exposes no
		// batched datagram send.
		written := 0
		for _, segment := range vec {
			if len(segment) == 0 {
				continue
			}
			n, err := fc.Write(segment)
			written += n
			if err != nil {
				return written, err
			}
		}
		return written, nil
	}

	total := 0
	segments := 0
	for _, segment := range vec {
		if len(segment) > int(^uint(0)>>1)-total {
			return 0, ErrOutboundOverflow
		}
		total += len(segment)
		if len(segment) > 0 {
			segments++
		}
	}
	if total == 0 {
		return 0, nil
	}
	if total >= stdOwnedWriteThreshold {
		if limit := fc.payloadLimit(); limit > 0 && total > limit {
			return 0, ErrOutboundOverflow
		}
		var inline [8]*bytebuf.Buffer
		owned := inline[:0]
		if segments > len(inline) {
			owned = append(owned, bytebuf.CloneBuffers(vec, total))
		} else {
			for _, segment := range vec {
				if len(segment) > 0 {
					owned = append(owned, bytebuf.CloneBuffer(segment))
				}
			}
		}
		fc.mux.Lock()
		if fc.isClosing() || fc.draining.Load() || fc.writeShut {
			fc.mux.Unlock()
			releaseStdBuffers(owned)
			return 0, net.ErrClosed
		}
		if limit := fc.payloadLimit(); limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-total {
			fc.mux.Unlock()
			releaseStdBuffers(owned)
			return 0, ErrOutboundOverflow
		}
		for _, buffer := range owned {
			fc.outbound.AppendOwned(buffer)
		}
		fc.mux.Unlock()
		select {
		case fc.writeSig <- struct{}{}:
		default:
		}
		return total, nil
	}
	fc.mux.Lock()
	if fc.isClosing() || fc.draining.Load() || fc.writeShut {
		fc.mux.Unlock()
		return 0, net.ErrClosed
	}
	if limit := fc.payloadLimit(); limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-total {
		fc.mux.Unlock()
		return 0, ErrOutboundOverflow
	}
	n = fc.outbound.WritevCoalesced(vec, fc.coalesceBlockSize())
	fc.mux.Unlock()

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}

	return
}

func (fc *fdConn) WriteOwned(owned *Buffer) (n int, err error) {
	if owned != nil && fc.udp == nil && fc.entry != nil && fc.entry.directConsumer && fc.events().BlockingWrite {
		if fc.hasWaitingBlockWriter() && !blockio.OnTurn() {
			return fc.ensureBlockWriter().WriteOwned(owned)
		}
		n, err = fc.writeOwnedNonBlocking(owned)
		if errors.Is(err, ErrOutboundOverflow) && !blockio.OnTurn() {
			return fc.ensureBlockWriter().WriteOwned(owned)
		}
		return n, err
	}
	return fc.writeOwnedNonBlocking(owned)
}

func (fc *fdConn) writeOwnedNonBlocking(owned *Buffer) (n int, err error) {
	if owned == nil {
		return 0, nil
	}
	size := owned.Len()
	if size == 0 {
		bytebuf.ReleaseBuffer(owned)
		return 0, nil
	}
	if fc.isClosing() {
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if fc.udp != nil {
		defer bytebuf.ReleaseBuffer(owned)
		return fc.Write(owned.Bytes())
	}

	fc.mux.Lock()
	if fc.isClosing() || fc.draining.Load() || fc.writeShut {
		fc.mux.Unlock()
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if limit := fc.payloadLimit(); limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-size {
		fc.mux.Unlock()
		// An overflowed buffer was never accepted, so it remains the caller's.
		return 0, ErrOutboundOverflow
	}
	fc.outbound.AppendOwned(owned)
	fc.mux.Unlock()

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}
	return size, nil
}

func (fc *fdConn) Flush() error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	// Writes are FIFO in outbound. A coalesced wake is enough to make the
	// dedicated writer drain everything accepted before this call.
	select {
	case fc.writeSig <- struct{}{}:
	default:
	}
	return nil
}

// stdWriteScratch is the fixed scratch one drain fills and hands over. It
// lives in a pool rather than on the writer goroutine's stack: both the
// vector and the net.Buffers wrapper reach net.Buffers.WriteTo as values
// whose address the compiler must assume escapes, which would move the vector
// to the heap once per connection, for the connection's whole life, and
// allocate the wrapper once per drain. Pooled, an idle connection holds
// neither.
type stdWriteScratch struct {
	vec  [stdWriteVecLimit][]byte
	bufs net.Buffers
}

// drainOutbound swaps the producer buffer into writeBatch before blocking on
// the network. Producers immediately continue on a fresh CompositeBuffer;
// writeMu prevents close cleanup from returning in-flight blocks to the pool.
func (fc *fdConn) drainOutbound() (int, error) {
	scratch := stdWriteScratchPool.Get().(*stdWriteScratch)
	defer func() {
		clear(scratch.vec[:])
		scratch.bufs = nil
		stdWriteScratchPool.Put(scratch)
	}()
	fc.writeMu.Lock()
	defer fc.writeMu.Unlock()

	fc.mux.Lock()
	if fc.isClosing() {
		fc.mux.Unlock()
		return 0, net.ErrClosed
	}
	fc.writeBytes = fc.outbound.Len()
	if fc.writeBytes == 0 {
		fc.mux.Unlock()
		if fc.draining.Load() {
			// A graceful close waits for an empty queue; the writer just
			// found one, or drained the last of it elsewhere.
			fc.finishDrain()
		}
		return 0, nil
	}
	// The detached batch is immutable while producers append to a fresh buffer.
	fc.outbound, fc.writeBatch = fc.writeBatch, fc.outbound
	fc.mux.Unlock()

	totalWritten := 0
	var writeErr error
	for !fc.writeBatch.Empty() {
		buffers, size := fc.writeBatch.PeekVecN(scratch.vec[:], len(scratch.vec))
		scratch.bufs = buffers
		written, err := scratch.bufs.WriteTo(fc.conn)
		clear(buffers)
		if written > 0 {
			n := int(written)
			fc.writeBatch.Discard(n)
			totalWritten += n
			fc.mux.Lock()
			fc.writeBytes -= n
			if fc.draining.Load() && fc.drainTimer != nil {
				// A drain's queue is moving: its bound starts over, as the
				// native backend's does when bytes leave the socket.
				fc.drainTimer.Reset(fc.drainWait)
			}
			fc.mux.Unlock()
		}
		if err != nil {
			writeErr = err
			break
		}
		if written < int64(size) {
			writeErr = io.ErrShortWrite
			break
		}
	}
	fc.writeBatch.Reset()
	fc.mux.Lock()
	fc.writeBytes = 0
	empty := fc.draining.Load() && fc.outbound.Len() == 0
	fc.mux.Unlock()
	if empty {
		// The writer drained everything queued: a graceful close's wait is
		// over. Producers are refused while draining, so nothing follows.
		fc.finishDrain()
	}
	return totalWritten, writeErr
}
