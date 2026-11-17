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
	"io"
	"net"
	"syscall"

	"github.com/urpc/uio/internal/socket"
)

// sendUDPOutside sends one datagram from outside the connection's turn. A
// datagram is never queued, so the send happens here, holding the descriptor
// owner's submitMu, which keeps the socket from closing under the syscall;
// OnBytesWritten and a failure's close run after the lock is released. The lock
// also orders the send before a later Close or Abort.
func (conn *fdConn) sendUDPOutside(data []byte) (int, error) {
	if limit := conn.payloadLimit(); limit > 0 && len(data) > limit {
		return 0, conn.errOutboundOverflow()
	}
	owner := conn.descriptorOwner()
	owner.submitMu.Lock()
	if conn.loop == nil || conn.isClosing() || owner.close.isReleased() || conn.events().closing.Load() || conn.loop.stopping.Load() {
		owner.submitMu.Unlock()
		return 0, net.ErrClosed
	}
	gate, open := conn.enterDescriptor()
	if !open {
		owner.submitMu.Unlock()
		return 0, net.ErrClosed
	}
	written, err := conn.sendDatagram(data)
	gate.leave()
	owner.submitMu.Unlock()
	return conn.finishDatagram(data, written, err)
}

// writevUDP sends each buffer as one datagram, batched into one sendmmsg
// where the system has it. A datagram the socket refuses is lost, as a
// datagram transport allows: the protocol above retransmits what it needs.
// The count reports the bytes of the datagrams the socket took.
func (conn *fdConn) writevUDP(vec [][]byte) (int, error) {
	if limit := conn.payloadLimit(); limit > 0 {
		for _, segment := range vec {
			if len(segment) > limit {
				return 0, conn.errOutboundOverflow()
			}
		}
	}
	owner := conn.descriptorOwner()
	owner.submitMu.Lock()
	if conn.loop == nil || conn.isClosing() || owner.close.isReleased() || conn.events().closing.Load() || conn.loop.stopping.Load() {
		owner.submitMu.Unlock()
		return 0, net.ErrClosed
	}
	gate, open := conn.enterDescriptor()
	if !open {
		owner.submitMu.Unlock()
		return 0, net.ErrClosed
	}
	written, err := socket.SendUDPBatch(int(conn.fd), vec, conn.udp.remote)
	gate.leave()
	owner.submitMu.Unlock()
	conn.reportOutbound(written)
	if err != nil {
		if isUDPSendBlocked(err) {
			return written, err
		}
		conn.requestClose(err)
		return written, err
	}
	return written, nil
}

// sendUDPOnLoop sends one datagram from the connection's turn. Sends from
// other goroutines share the socket and its destination address, so every
// send holds the descriptor owner's submitMu.
func (conn *fdConn) sendUDPOnLoop(data []byte) (written int, err error) {
	owner := conn.descriptorOwner()
	owner.submitMu.Lock()
	gate, open := conn.enterDescriptor()
	if !open {
		owner.submitMu.Unlock()
		return 0, net.ErrClosed
	}
	written, err = conn.sendDatagram(data)
	gate.leave()
	owner.submitMu.Unlock()
	return conn.finishDatagram(data, written, err)
}

// sendDatagram is the send syscall alone. Its caller holds the descriptor
// owner's submitMu: Sendto encodes the destination into the address value
// it is given, which every send to this peer shares.
func (conn *fdConn) sendDatagram(data []byte) (written int, err error) {
	if conn.udp.remote == nil {
		written, err = syscall.Write(int(conn.fd), data)
	} else {
		err = syscall.Sendto(int(conn.fd), data, 0, conn.udp.remote)
		if err == nil {
			written = len(data)
		}
	}
	if written < 0 {
		written = 0
	}
	return written, err
}

// finishDatagram preserves datagram atomicity. A blocked datagram is reported
// to the caller rather than queued as a stream suffix, and a partial result is
// fatal because retrying it would create a different packet.
func (conn *fdConn) finishDatagram(data []byte, written int, err error) (int, error) {
	conn.reportOutbound(written)
	if err != nil {
		if isUDPSendBlocked(err) {
			return written, err
		}
		conn.requestClose(err)
		return written, err
	}
	if written != len(data) {
		// Datagram boundaries are atomic; never retry a partial suffix as a stream.
		err = io.ErrShortWrite
		conn.requestClose(err)
		return written, err
	}
	return written, nil
}

// reportOutbound runs OnBytesWritten for a datagram send. Datagrams are sent on
// whichever goroutine writes them, so the calls for one connection go through
// a claim, as a stream's sends do: a sender adds its bytes and reports them
// itself if it takes the claim, and otherwise leaves them to the holder, which
// reports until nothing is pending, so the calls never overlap and a send made
// inside OnBytesWritten is reported once that call returns. Nobody waits for the
// claim, so OnBytesWritten callbacks that write to each other cannot deadlock.
func (conn *fdConn) reportOutbound(n int) {
	if n <= 0 || (conn.callbacks().onBytesWritten == nil && !conn.hasWriteControl()) {
		return
	}
	udp := conn.udp
	udp.outboundPending.Add(int64(n))
	for udp.outboundClaim.CompareAndSwap(false, true) {
		conn.drainOutbound()
		// Bytes added between the last drain and the release found the
		// claim taken; look again.
		if udp.outboundPending.Load() == 0 {
			return
		}
	}
}

// drainOutbound reports pending bytes while holding the claim, and releases it
// even when OnBytesWritten panics.
func (conn *fdConn) drainOutbound() {
	udp := conn.udp
	defer udp.outboundClaim.Store(false)
	for {
		pending := udp.outboundPending.Swap(0)
		if pending == 0 {
			return
		}
		conn.events().reportBytesWritten(conn, int(pending))
	}
}

func isUDPSendBlocked(err error) bool {
	return isWouldBlock(err) || err == syscall.ENOBUFS
}
