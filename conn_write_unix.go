//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"unsafe"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/socket"
)

// nativeWriteVecLimit bounds stack use while allowing a read event's small
// owned writes to drain in one syscall.
const nativeWriteVecLimit = 64

func (conn *fdConn) WriteByte(value byte) error {
	var data [1]byte
	data[0] = value
	_, err := conn.Write(data[:])
	return err
}

func (conn *fdConn) WriteString(value string) (int, error) {
	data := unsafe.Slice(unsafe.StringData(value), len(value))
	return conn.Write(data)
}

func (conn *fdConn) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if conn.isClosing() {
		return 0, net.ErrClosed
	}
	if conn.directOwner() {
		return conn.writeOnLoop(data)
	}
	if conn.isDatagram() {
		owned := bytebuf.CloneBuffer(data)
		n, err := conn.queueUDPWrite(owned, len(data))
		if errors.Is(err, ErrOutboundOverflow) {
			// The clone never entered the queue, so Write still owns it.
			bytebuf.ReleaseBuffer(owned)
		}
		return n, err
	}
	// This fast rejection belongs after the direct path: data sent straight to
	// the kernel never counts against the user-space payload limit.
	if limit := conn.events.MaxOutboundBuffered; limit > 0 && len(data) > limit {
		return 0, ErrOutboundOverflow
	}
	if err := conn.precheckOutbound(len(data)); err != nil {
		return 0, err
	}
	owned := bytebuf.CloneBuffer(data)
	n, err := conn.queueOwnedWrite(owned, len(data))
	if errors.Is(err, ErrOutboundOverflow) {
		// The clone never entered the queue, so Write still owns it.
		bytebuf.ReleaseBuffer(owned)
	}
	return n, err
}

func (conn *fdConn) Writev(vec [][]byte) (int, error) {
	total := 0
	for _, segment := range vec {
		if len(segment) > int(^uint(0)>>1)-total {
			return 0, ErrOutboundOverflow
		}
		total += len(segment)
	}
	if total == 0 {
		return 0, nil
	}
	if conn.isClosing() {
		return 0, net.ErrClosed
	}
	if conn.isDatagram() {
		return 0, errUnsupported
	}
	if conn.directOwner() {
		return conn.writevOnLoop(vec, total)
	}
	if limit := conn.events.MaxOutboundBuffered; limit > 0 && total > limit {
		return 0, ErrOutboundOverflow
	}
	if err := conn.precheckOutbound(total); err != nil {
		return 0, err
	}
	owned := bytebuf.CloneBuffers(vec, total)
	n, err := conn.queueOwnedWrite(owned, total)
	if errors.Is(err, ErrOutboundOverflow) {
		// The clone never entered the queue, so Writev still owns it.
		bytebuf.ReleaseBuffer(owned)
	}
	return n, err
}

func (conn *fdConn) WriteOwned(owned *Buffer) (int, error) {
	if owned == nil {
		return 0, nil
	}
	size := owned.Len()
	if size == 0 {
		bytebuf.ReleaseBuffer(owned)
		return 0, nil
	}
	if conn.isClosing() {
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if conn.directOwner() {
		if conn.isDatagram() {
			defer bytebuf.ReleaseBuffer(owned)
			return conn.sendUDPOnLoop(owned.Bytes())
		}
		return conn.writeOwnedOnLoop(owned, size)
	}
	if conn.isDatagram() {
		return conn.queueUDPWrite(owned, size)
	}
	return conn.queueOwnedWrite(owned, size)
}

// ReserveOutbound is the task-owner encoding path: the reserved bytes are part
// of outbound as soon as it returns, and the caller fills them after submitMu
// is released. No other sender may take outbound meanwhile, so the turn keeps
// the write claim until it ends; while another sender holds the claim the
// reservation is refused and the caller writes another way.
func (conn *fdConn) ReserveOutbound(n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	if conn.isClosing() {
		return nil, net.ErrClosed
	}
	if conn.isDatagram() || !conn.directOwner() {
		return nil, ErrReserveUnsupported
	}
	took := !conn.turnHoldsWrite()
	if took && !conn.tryClaimWrite(writeTurnHeldFlag) {
		return nil, ErrReserveUnsupported
	}
	reserved, err := conn.reservePendingAfterFlush(int64(n))
	if err == nil && !reserved {
		err = ErrOutboundOverflow
	}
	if err != nil {
		if took {
			// Nothing was reserved, so other senders need not wait for this
			// turn's next flush.
			conn.releaseWriteAndKick()
		}
		return nil, err
	}
	conn.submitMu.Lock()
	buffer := conn.outbound.Reserve(n, conn.coalesceBlockSize())
	conn.submitMu.Unlock()
	return buffer, nil
}

// coalesceBlockSize is the block size that small writes batched in one task
// share: one read round's replies usually fit in one of them.
func (conn *fdConn) coalesceBlockSize() int {
	return min(64<<10, conn.events.readBufferSize*2)
}

// queueUDPWrite transfers one datagram to the owning loop and waits for the
// nonblocking send result. The admission lock also orders it before a later
// CloseWith. A callback on another loop cannot wait without risking a cycle.
func (conn *fdConn) queueUDPWrite(owned *Buffer, size int) (int, error) {
	if isEventLoopGoroutine() {
		bytebuf.ReleaseBuffer(owned)
		return 0, ErrUDPWriteOnEventLoop
	}
	t := acquireTask(udpWriteTask, conn)
	t.udpPayload = owned
	t.udpDone = make(chan udpWriteResult, 1)
	done := t.udpDone
	conn.submitMu.Lock()
	if conn.loop == nil || conn.events == nil || conn.isClosing() || conn.events.closing.Load() || conn.loop.stopping.Load() {
		conn.submitMu.Unlock()
		bytebuf.ReleaseBuffer(owned)
		releaseTask(t)
		return 0, net.ErrClosed
	}
	if !conn.reservePending(int64(size)) {
		conn.submitMu.Unlock()
		releaseTask(t)
		// The datagram was not queued and not sent: ownership returns to the
		// caller like every other ErrOutboundOverflow.
		return 0, ErrOutboundOverflow
	}
	if !conn.loop.pushTask(t) {
		conn.pending.Add(-int64(size))
		conn.submitMu.Unlock()
		bytebuf.ReleaseBuffer(owned)
		releaseTask(t)
		return 0, net.ErrClosed
	}
	conn.submitMu.Unlock()
	conn.loop.notify()
	result := <-done
	return result.n, result.err
}

func (conn *fdConn) settleUDPWrite(size int64) {
	for {
		pending := conn.pending.Load()
		if pending == 0 {
			return // closeOnLoop already cleared the connection's accounting.
		}
		remaining := pending - size
		if remaining < 0 {
			remaining = 0
		}
		if conn.pending.CompareAndSwap(pending, remaining) {
			return
		}
	}
}

func (conn *fdConn) runUDPWriteTask(owned *Buffer) udpWriteResult {
	defer bytebuf.ReleaseBuffer(owned)
	defer conn.settleUDPWrite(int64(owned.Len()))
	if conn.isClosedOnLoop() || (conn.udp.server != nil && conn.udp.server.isClosedOnLoop()) {
		return udpWriteResult{err: net.ErrClosed}
	}
	n, err := conn.sendUDPOnLoop(owned.Bytes())
	return udpWriteResult{n: n, err: err}
}

func (conn *fdConn) precheckOutbound(size int) error {
	if limit := int64(conn.events.MaxOutboundBuffered); limit > 0 {
		pending := conn.pending.Load()
		if int64(size) > limit-pending {
			return ErrOutboundOverflow
		}
	}
	return nil
}

// queueOwnedWrite is the cross-goroutine write path. Ownership has already
// moved into owned, so submitMu only covers admission, accounting, and pointer
// insertion; payload allocation and copying never happen under the lock. The
// bytes are sent by a write turn, beside whatever the connection's turn is
// reading. ErrOutboundOverflow accepts nothing and returns owned to the caller.
func (conn *fdConn) queueOwnedWrite(owned *bytebuf.Buffer, size int) (int, error) {
	if limit := conn.events.MaxOutboundBuffered; limit > 0 && size > limit {
		return 0, ErrOutboundOverflow
	}
	// Allocation and the only payload copy have already happened off-lock.
	conn.submitMu.Lock()
	if conn.loop == nil || conn.isClosing() || conn.events.closing.Load() || conn.loop.stopping.Load() {
		conn.submitMu.Unlock()
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if !conn.reservePending(int64(size)) {
		conn.submitMu.Unlock()
		return 0, ErrOutboundOverflow
	}
	conn.outbound.AppendOwned(owned)
	conn.submitMu.Unlock()
	conn.kickWriter()
	return size, nil
}

func (conn *fdConn) reservePending(size int64) bool {
	// Both callback partial writes and external producers reserve this counter.
	limit := int64(conn.events.MaxOutboundBuffered)
	if limit <= 0 {
		conn.pending.Add(size)
		return true
	}
	for {
		old := conn.pending.Load()
		if size > limit-old {
			return false
		}
		if conn.pending.CompareAndSwap(old, old+size) {
			return true
		}
	}
}

func (conn *fdConn) reservePendingAfterFlush(size int64) (bool, error) {
	if conn.reservePending(size) {
		return true, nil
	}
	if conn.outboundEmpty() || conn.writeBlocked() {
		return false, nil
	}
	if _, err := conn.flushOnLoop(); err != nil {
		return false, err
	}
	return conn.reservePending(size), nil
}

// claimDirectSend reports whether the turn may send directly, holding the
// claim it took for that. A claim the turn keeps for a reservation, or holds
// for its own flush while OnOutbound runs, is never lent to a direct send: the
// bytes are queued and that flush sends them, so OnOutbound never runs inside
// itself. Once it holds the claim the turn must also find nothing queued:
// another sender may have left part of its output there and released the
// claim between the turn's check and its taking it, and those bytes go first.
// A direct send therefore starts with nothing queued or in flight, which is
// what lets it put an unsent suffix at the head of outbound.
func (conn *fdConn) claimDirectSend() bool {
	if !conn.tryClaimWrite(0) {
		// Another sender, or this turn's own flush or reservation, owns the
		// socket; its release or flush sends these bytes.
		return false
	}
	if !conn.outboundEmpty() {
		conn.releaseWriteAndKick()
		return false
	}
	return true
}

// writeOnLoop is the task-owner fast path. It lends caller memory directly to
// the non-blocking syscall when no batching is active and copies only an unsent
// suffix that must outlive the call.
func (conn *fdConn) writeOnLoop(data []byte) (int, error) {
	if conn.isDatagram() {
		return conn.sendUDPOnLoop(data)
	}
	threshold := conn.events.WriteBufferedThreshold
	queued := !conn.outboundEmpty() || conn.turn&turnCorked != 0 || (threshold > 0 && len(data) < threshold)
	if !queued && !conn.claimDirectSend() {
		queued = true
	}
	if queued {
		// Batching or an existing tail requires one copy into connection-owned storage.
		reserved, err := conn.reservePendingAfterFlush(int64(len(data)))
		if err != nil {
			return 0, err
		}
		if !reserved {
			return 0, ErrOutboundOverflow
		}
		conn.submitMu.Lock()
		_, _ = conn.outbound.Write(data)
		conn.submitMu.Unlock()
		return len(data), nil
	}
	defer conn.releaseWriteAndKick()

	// The common callback path lends caller memory directly to the kernel.
	written, err := socket.Send(conn.fd, data)
	if written < 0 {
		written = 0
	}
	if err != nil {
		if isWouldBlock(err) {
			written, err = 0, nil
		} else {
			conn.failDirectWrite(err)
			return written, err
		}
	}
	if written == len(data) {
		conn.events.onSocketBytesWrite(conn, written)
		return written, nil
	}
	remaining := data[written:]
	if !conn.reservePending(int64(len(remaining))) {
		if written == 0 {
			// Nothing left the socket: pure overflow.
			return 0, ErrOutboundOverflow
		}
		// A partial payload is on the wire and its suffix cannot be queued:
		// the stream is broken, so report a short write, not an overflow.
		conn.events.onSocketBytesWrite(conn, written)
		conn.failDirectWrite(ErrOutboundOverflow)
		return written, io.ErrShortWrite
	}
	// Only the unsent suffix must survive after Write returns.
	conn.queueDirectSuffix(bytebuf.CloneBuffer(remaining))
	conn.setWriteBlocked(written == 0)
	// Reported once the suffix is accounted, so OnOutbound never observes a
	// transient empty queue in the middle of this write.
	conn.events.onSocketBytesWrite(conn, written)
	return len(data), nil
}

// writevOnLoop mirrors writeOnLoop for scatter/gather input. A partial syscall
// is collapsed into one owned suffix so later retries do not retain caller
// slices or an unbounded vector list.
func (conn *fdConn) writevOnLoop(vec [][]byte, total int) (int, error) {
	if conn.isDatagram() {
		return 0, errUnsupported
	}
	threshold := conn.events.WriteBufferedThreshold
	queued := !conn.outboundEmpty() || conn.turn&turnCorked != 0 || (threshold > 0 && total < threshold)
	if !queued && !conn.claimDirectSend() {
		queued = true
	}
	if queued {
		reserved, err := conn.reservePendingAfterFlush(int64(total))
		if err != nil {
			return 0, err
		}
		if !reserved {
			return 0, ErrOutboundOverflow
		}
		conn.submitMu.Lock()
		conn.outbound.WritevCoalesced(vec, conn.coalesceBlockSize())
		conn.submitMu.Unlock()
		return total, nil
	}
	defer conn.releaseWriteAndKick()
	written, err := socket.Writev(conn.fd, vec)
	if written < 0 {
		written = 0
	}
	if err != nil {
		if isWouldBlock(err) {
			written, err = 0, nil
		} else {
			conn.failDirectWrite(err)
			return written, err
		}
	}
	if written == total {
		conn.events.onSocketBytesWrite(conn, written)
		return written, nil
	}
	remaining := total - written
	if !conn.reservePending(int64(remaining)) {
		if written == 0 {
			// Nothing left the socket: pure overflow.
			return 0, ErrOutboundOverflow
		}
		// A partial vector is on the wire and its suffix cannot be queued:
		// the stream is broken, so report a short write, not an overflow.
		conn.events.onSocketBytesWrite(conn, written)
		conn.failDirectWrite(ErrOutboundOverflow)
		return written, io.ErrShortWrite
	}
	owned := bytebuf.CloneBuffersFrom(vec, written, remaining)
	conn.queueDirectSuffix(owned)
	conn.setWriteBlocked(written == 0)
	conn.events.onSocketBytesWrite(conn, written)
	return total, nil
}

// writeOwnedOnLoop consumes owned on every return path except
// ErrOutboundOverflow, which accepts nothing and returns the buffer to the
// caller; a partial direct write that cannot queue its suffix reports
// io.ErrShortWrite and consumes the buffer instead. During a corked read
// round, the first small frame keeps zero-copy ownership and later frames are
// coalesced into pooled blocks to keep the final writev batch short. Buffers
// up to half a coalescing block are copied, which also keeps a queue that the
// peer is slow to drain from holding many mostly empty blocks; a larger buffer
// already carries many frames and keeps its own writev segment.
func (conn *fdConn) writeOwnedOnLoop(owned *bytebuf.Buffer, size int) (int, error) {
	threshold := conn.events.WriteBufferedThreshold
	queued := !conn.outboundEmpty() || conn.turn&turnCorked != 0 || (threshold > 0 && size < threshold)
	if !queued && !conn.claimDirectSend() {
		queued = true
	}
	if queued {
		reserved, err := conn.reservePendingAfterFlush(int64(size))
		if err != nil {
			bytebuf.ReleaseBuffer(owned)
			return 0, err
		}
		if !reserved {
			return 0, ErrOutboundOverflow
		}
		conn.submitMu.Lock()
		if conn.turn&turnCorked != 0 && !conn.outbound.Empty() && size <= conn.coalesceBlockSize()/2 {
			conn.outbound.AppendOwnedCoalesced(owned, conn.coalesceBlockSize())
		} else {
			conn.outbound.AppendOwned(owned)
		}
		conn.submitMu.Unlock()
		return size, nil
	}
	defer conn.releaseWriteAndKick()

	written, err := socket.Send(conn.fd, owned.Bytes())
	if written < 0 {
		written = 0
	}
	if err != nil {
		if isWouldBlock(err) {
			written, err = 0, nil
		} else {
			bytebuf.ReleaseBuffer(owned)
			conn.failDirectWrite(err)
			return written, err
		}
	}
	if written == size {
		bytebuf.ReleaseBuffer(owned)
		conn.events.onSocketBytesWrite(conn, written)
		return written, nil
	}
	remaining := size - written
	if !conn.reservePending(int64(remaining)) {
		if written == 0 {
			// Nothing left the socket: pure overflow, so the buffer returns
			// to the caller like every other ErrOutboundOverflow.
			return 0, ErrOutboundOverflow
		}
		// A partial frame is on the wire and its suffix cannot be queued:
		// the stream is broken, so the buffer is consumed and the caller is
		// told the write was short rather than merely overflowed.
		bytebuf.ReleaseBuffer(owned)
		conn.events.onSocketBytesWrite(conn, written)
		conn.failDirectWrite(ErrOutboundOverflow)
		return written, io.ErrShortWrite
	}
	if written > 0 {
		owned.Discard(written)
	}
	conn.queueDirectSuffix(owned)
	conn.setWriteBlocked(written == 0)
	conn.events.onSocketBytesWrite(conn, written)
	return size, nil
}

// queueDirectSuffix queues what a direct send left unsent at the head of
// outbound. The sender holds the write claim and started with nothing queued
// or in flight, so bytes in outbound now came from producers that wrote during
// the send; behind them, they would land inside this payload on the wire.
func (conn *fdConn) queueDirectSuffix(owned *bytebuf.Buffer) {
	conn.submitMu.Lock()
	conn.outbound.PrependOwned(owned)
	conn.submitMu.Unlock()
}

// sendUDPOnLoop preserves datagram atomicity. A blocked datagram is reported to
// the caller rather than queued as a stream suffix, and a partial result is
// fatal because retrying it would create a different packet.
func (conn *fdConn) sendUDPOnLoop(data []byte) (written int, err error) {
	if conn.udp.remote == nil {
		written, err = syscall.Write(conn.fd, data)
	} else {
		err = syscall.Sendto(conn.fd, data, 0, conn.udp.remote)
		if err == nil {
			written = len(data)
		}
	}
	if written < 0 {
		written = 0
	}
	conn.events.onSocketBytesWrite(conn, written)
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

func isUDPSendBlocked(err error) bool {
	return isWouldBlock(err) || err == syscall.ENOBUFS
}

func (conn *fdConn) failDirectWrite(err error) {
	// Once a syscall has made the byte stream unusable, queued writes must not
	// be appended ahead of the close task or flushed during close.
	conn.markWriteFailed()
	conn.requestClose(err)
}

func (conn *fdConn) Flush() error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.loop == nil || conn.loop.stopping.Load() {
		return net.ErrClosed
	}
	if conn.isDatagram() {
		return nil
	}
	if conn.directOwner() {
		_, err := conn.flushOnLoop()
		return err
	}
	// Outside the task, Flush is a FIFO barrier and returns after giving the
	// queued bytes a sender.
	conn.kickWriter()
	return nil
}

// inflightPool lends flushes the buffer that holds a detached batch, so an idle
// connection keeps no second queue.
var inflightPool = sync.Pool{New: func() any { return new(bytebuf.CompositeBuffer) }}

// releaseInflight returns the borrowed batch. Only the holder of the write
// claim calls it; close keeps the claim it takes, so nothing sends afterwards.
func (conn *fdConn) releaseInflight() {
	if batch := conn.inflight; batch != nil {
		conn.inflight = nil
		batch.Reset()
		inflightPool.Put(batch)
	}
}

// flushOnLoop is the connection turn's flush. The turn sends inline when it
// holds or can take the write claim, so replies produced by its callbacks
// leave without a scheduler hop; when another sender holds the claim, that
// sender's release rechecks and these bytes go with it.
func (conn *fdConn) flushOnLoop() (int, error) {
	// pending is incremented before a producer publishes its buffer and
	// decremented only after the corresponding bytes are sent. A zero
	// value therefore proves that there is no stream payload to flush, while
	// avoiding any atomic read-modify-write on the common empty path.
	if conn.isDatagram() || conn.pending.Load() == 0 {
		return 0, nil
	}
	// OnOutbound runs inside the flush, so the turn marks it: a Write that
	// callback makes is queued rather than sent, and a flush it starts returns
	// at once, so the callback never runs inside itself. A claim taken here
	// is marked too, so the turn's cleanup releases it if the callback panics.
	if conn.turn&turnFlushing != 0 {
		return 0, nil
	}
	held := conn.writeState.Load()&writeTurnHeldFlag != 0
	if !held && !conn.tryClaimWrite(writeTurnFlushFlag) {
		return 0, nil
	}
	conn.turn |= turnFlushing
	n, err := conn.flushWrite()
	conn.turn &^= turnFlushing
	if !held {
		conn.releaseWriteAndKick()
	}
	return n, err
}

// flushWrite drains a bounded number of bytes and writev calls. It requires
// the write claim, and submitMu never covers a send, so a producer appending
// to outbound never waits for the socket. A queue of one block, the usual
// shape after a round of replies, is sent in place: producers only append,
// into the tail block's spare room or as new blocks, and only the claim holder
// discards, so the bytes a send reads stay put while the lock is released.
// A longer queue is detached into inflight in one step instead, so its sends
// and the release of its sent blocks also run without the lock while producers
// start the next batch; inflight goes out ahead of everything accepted since.
// EAGAIN leaves the bytes queued and sets writeBlocked until a writable edge.
func (conn *fdConn) flushWrite() (int, error) {
	if conn.pending.Load() == 0 {
		return 0, nil
	}
	if conn.writeBlocked() {
		// Once EAGAIN is observed, only a Writable event should retry the fd.
		return 0, nil
	}
	var totalWritten int
	var writeErr error
	if conn.inflight == nil {
		conn.submitMu.Lock()
		if conn.outbound.Blocks() == 1 {
			totalWritten, writeErr = conn.sendOutboundBlock()
		}
		if writeErr != nil || conn.writeBlocked() || conn.outbound.Empty() {
			conn.submitMu.Unlock()
			return conn.retireFlush(totalWritten, writeErr)
		}
		batch := inflightPool.Get().(*bytebuf.CompositeBuffer)
		// batch is empty, so the swap leaves outbound empty for producers.
		conn.outbound, *batch = *batch, conn.outbound
		conn.submitMu.Unlock()
		conn.inflight = batch
	}
	var written int
	if conn.inflight.Blocks() == 1 {
		written, writeErr = conn.sendInflightBlock()
	} else {
		written, writeErr = conn.sendInflightVec()
	}
	totalWritten += written
	if conn.inflight.Empty() {
		conn.releaseInflight()
	}
	return conn.retireFlush(totalWritten, writeErr)
}

// retireFlush accounts what a flush sent and reports it.
func (conn *fdConn) retireFlush(written int, err error) (int, error) {
	if written > 0 {
		conn.retireSent(written)
		conn.events.onSocketBytesWrite(conn, written)
	}
	return written, err
}

// retireSent retires bytes the socket took. The decrement that takes the
// backlog down to the resume mark lets reads the outbound limit paused
// continue. Producers only add to pending and every add and retire is atomic,
// so each downward crossing is seen here exactly once, whoever is sending and
// whatever producers append meanwhile.
func (conn *fdConn) retireSent(n int) {
	after := conn.pending.Add(-int64(n))
	if limit := int64(conn.events.MaxOutboundBuffered); limit > 0 &&
		after <= limit/2 && after+int64(n) > limit/2 {
		conn.scheduleRefresh()
	}
}

// sendOutboundBlock sends a queue of one block in place, with plain write and
// no vector. It is called and returns with submitMu held, releases it around
// each send, and stops once producers have queued a second block.
func (conn *fdConn) sendOutboundBlock() (int, error) {
	totalWritten := 0
	for calls := 0; calls < 16 && conn.outbound.Blocks() == 1; calls++ {
		chunk := conn.outbound.PeekChunk()
		conn.submitMu.Unlock()
		written, err := socket.Send(conn.fd, chunk)
		conn.submitMu.Lock()
		if err != nil && !isWouldBlock(err) {
			return totalWritten, err
		}
		if err != nil || written <= 0 {
			if conn.markWriteBlocked() {
				continue
			}
			return totalWritten, nil
		}
		conn.outbound.Discard(written)
		totalWritten += written
	}
	return totalWritten, nil
}

// sendInflightBlock sends a detached batch of one block with plain write.
func (conn *fdConn) sendInflightBlock() (int, error) {
	totalWritten := 0
	for calls := 0; calls < 16 && !conn.inflight.Empty(); calls++ {
		written, err := socket.Send(conn.fd, conn.inflight.PeekChunk())
		if err != nil && !isWouldBlock(err) {
			return totalWritten, err
		}
		if err != nil || written <= 0 {
			if conn.markWriteBlocked() {
				continue
			}
			return totalWritten, nil
		}
		conn.inflight.Discard(written)
		totalWritten += written
	}
	return totalWritten, nil
}

// sendInflightVec drains a detached multi-block batch in bounded writev
// batches. Its vector is kept out of flushWrite's frame, which would otherwise
// clear it on every flush.
//
//go:noinline
func (conn *fdConn) sendInflightVec() (int, error) {
	var vecStorage [nativeWriteVecLimit][]byte
	totalWritten := 0
	for calls := 0; calls < 16 && totalWritten < 1<<20 && !conn.inflight.Empty(); calls++ {
		vec, _ := conn.inflight.PeekVecN(vecStorage[:0], len(vecStorage))
		written, err := socket.Writev(conn.fd, vec)
		if err != nil && !isWouldBlock(err) {
			return totalWritten, err
		}
		if err != nil || written <= 0 {
			if conn.markWriteBlocked() {
				continue
			}
			break
		}
		conn.inflight.Discard(written)
		totalWritten += written
	}
	return totalWritten, nil
}

func (conn *fdConn) outboundEmpty() bool {
	// Queue admission reserves pending bytes before appending the owned buffer;
	// flush retires the bytes only after removing them. This makes pending a
	// conservative, lock-free empty check for stream connections. Callers still
	// take submitMu before inspecting or mutating the segment list itself.
	return conn.pending.Load() == 0
}

// updateInterest runs the outbound limit's read-pause hysteresis on the loop,
// whose refresh command then redelivers a paused read once throttled is
// clear. A stream's registration never changes — read and write interest are
// armed together at Add, and under edge triggering the always-armed write
// interest reports a socket only when it goes from full back to writable —
// so this issues no epoll_ctl.
func (conn *fdConn) updateInterest() error {
	if conn.close.isReleased() || (conn.udp != nil && conn.udp.server != nil) {
		return nil
	}
	if limit := int64(conn.events.MaxOutboundBuffered); limit > 0 {
		// Hysteresis avoids resuming around a single threshold.
		if pending := conn.pending.Load(); pending <= limit/2 {
			conn.throttled.Store(false)
		} else if pending >= limit-limit/4 {
			conn.throttled.Store(true)
		}
	} else {
		conn.throttled.Store(false)
	}
	return nil
}

// readShouldStop reports whether this connection filled its outbound limit,
// which ends a read round however far it got; a turn does not start reading
// past the 75% mark at all (see readPaused).
func (conn *fdConn) readShouldStop() bool {
	if limit := int64(conn.events.MaxOutboundBuffered); limit > 0 {
		return conn.pending.Load() >= limit
	}
	return false
}

func (conn *fdConn) OutboundBuffered() int { return int(conn.pending.Load()) }
