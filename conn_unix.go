//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
)

type fdConn struct {
	commonConn
	fd  int
	udp *unixUDPState // nil for stream connections

	// submitMu only orders cross-goroutine Write, Flush, and Close submissions.
	// It never protects socket I/O or outbound buffers.
	submitMu     sync.Mutex
	closing      atomic.Bool
	pending      atomic.Int64 // accepted payload in tasks plus outbound
	queuedWrites atomic.Int64 // write tasks not yet consumed by the loop

	// The remaining state is owned exclusively by conn.loop.
	closed           bool
	outbound         bytebuf.CompositeBuffer
	throttled        bool
	writeBlocked     bool
	writeFailed      bool
	touched          bool
	interest         poller.Interest
	deferredCloseErr *error

	deadlines *deadlineState // allocated by the first nonzero deadline
}

// unixUDPState contains fields that TCP connections never use.
type unixUDPState struct {
	file   *os.File // owns a duplicated UDP listener fd when non-nil
	remote syscall.Sockaddr
	server *fdConn
	peers  map[socket.UDPAddress]*fdConn
	key    socket.UDPAddress
}

// deadlineState remains loop-owned except for its atomic timer generations.
type deadlineState struct {
	readTimer       *time.Timer
	writeTimer      *time.Timer
	readGeneration  uint64
	writeGeneration uint64
	readTimerGen    atomic.Uint64
	writeTimerGen   atomic.Uint64
	readDeadline    time.Time
	writeDeadline   time.Time
}

func (conn *fdConn) Fd() int                              { return conn.fd }
func (conn *fdConn) initialInterest() poller.Interest     { return poller.Readable }
func (conn *fdConn) setInterest(interest poller.Interest) { conn.interest = interest }
func (conn *fdConn) currentInterest() poller.Interest     { return conn.interest }
func (conn *fdConn) isClosing() bool                      { return conn.closing.Load() }
func (conn *fdConn) isClosedOnLoop() bool                 { return conn.closed }
func (conn *fdConn) isDatagram() bool                     { return conn.udp != nil }
func (conn *fdConn) afterRegister()                       {}
func (conn *fdConn) clearTouched()                        { conn.touched = false }
func (conn *fdConn) markTouched() bool {
	if conn.touched {
		return false
	}
	conn.touched = true
	return true
}

func (conn *fdConn) closeUnregistered() {
	if conn.closing.CompareAndSwap(false, true) {
		conn.closed = true
		if conn.udp != nil && conn.udp.file != nil {
			_ = conn.udp.file.Close()
		} else {
			_ = syscall.Close(conn.fd)
		}
	}
}

func (conn *fdConn) SetLinger(seconds int) error {
	return conn.setSocketOption(optionLinger, seconds)
}
func (conn *fdConn) SetNoDelay(noDelay bool) error {
	return conn.setSocketOption(optionNoDelay, boolInt(noDelay))
}
func (conn *fdConn) SetReadBuffer(size int) error {
	return conn.setSocketOption(optionReadBuffer, size)
}
func (conn *fdConn) SetWriteBuffer(size int) error {
	return conn.setSocketOption(optionWriteBuffer, size)
}
func (conn *fdConn) SetKeepAlive(keepAlive bool) error {
	return conn.setSocketOption(optionKeepAlive, boolInt(keepAlive))
}
func (conn *fdConn) SetKeepAlivePeriod(seconds int) error {
	return conn.setSocketOption(optionKeepAlivePeriod, seconds)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (conn *fdConn) setSocketOption(kind socketOptionKind, value int) error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.loop.inLoop() {
		return conn.applySocketOption(kind, value)
	}
	// External callers wait for the loop result; no submission lock covers I/O.
	t := acquireTask(optionTask, conn)
	t.optionKind, t.optionValue = kind, value
	t.done = make(chan error, 1)
	done := t.done
	if !conn.loop.submitTask(t) {
		releaseTask(t)
		return net.ErrClosed
	}
	return <-done
}

func (conn *fdConn) applySocketOption(kind socketOptionKind, value int) error {
	if conn.closed {
		return net.ErrClosed
	}
	switch kind {
	case optionLinger:
		return socket.SetLinger(conn.fd, value)
	case optionNoDelay:
		return socket.SetNoDelay(conn.fd, value != 0)
	case optionKeepAlive:
		return socket.SetKeepAlive(conn.fd, value != 0)
	case optionKeepAlivePeriod:
		return socket.SetKeepAlivePeriod(conn.fd, value)
	case optionReadBuffer:
		return socket.SetRecvBuffer(conn.fd, value)
	default:
		return socket.SetSendBuffer(conn.fd, value)
	}
}

func (conn *fdConn) fireOnOpen() {
	if callback := conn.events.OnOpen; callback != nil {
		callback(conn)
	}
	if err := conn.finishCallback(); err != nil {
		conn.requestClose(err)
	}
}

func (conn *fdConn) fireOnData() error {
	var err error
	if callback := conn.events.OnData; callback != nil {
		err = callback(conn)
	} else {
		_, _ = conn.Discard(-1)
	}
	if err != nil {
		return err
	}
	return conn.finishCallback()
}

func (conn *fdConn) finishCallback() error {
	if conn.isClosing() || conn.closed {
		return nil
	}
	// The threshold batches within this callback; EAGAIN tails remain poller-driven.
	if _, err := conn.flushOnLoop(); err != nil {
		return err
	}
	return conn.updateInterest()
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
	conn.writeBlocked = false
	if _, err := conn.flushOnLoop(); err != nil {
		return err
	}
	return conn.updateInterest()
}

func (conn *fdConn) onRead() error {
	buffer := conn.loop.getBuffer()
	totalRead := 0
	for calls := 0; calls < 16 && totalRead < 1<<20; calls++ {
		n, err := syscall.Read(conn.fd, buffer)
		if err != nil {
			if isWouldBlock(err) {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.EOF
		}
		totalRead += n
		// inboundTail aliases the loop read buffer. Unconsumed bytes are copied
		// into inbound before the buffer is reused by the next syscall.
		conn.inboundTail = buffer[:n]
		conn.events.onSocketBytesRead(conn, n)
		if conn.isClosing() {
			conn.inboundTail = nil
			return nil
		}
		if err = conn.fireOnData(); err != nil {
			return err
		}
		if conn.isClosing() {
			conn.inboundTail = nil
			return nil
		}
		if len(conn.inboundTail) > 0 {
			if limit := conn.events.MaxInboundBuffered; limit > 0 && conn.InboundBuffered() > limit {
				conn.inboundTail = nil
				return ErrInboundOverflow
			}
			_, _ = conn.inbound.Write(conn.inboundTail)
			conn.inboundTail = nil
		}
		if n < len(buffer) {
			return nil
		}
	}
	return nil
}

func (conn *fdConn) onRecvUDP() error {
	buffer := conn.loop.getBuffer()
	totalRead := 0
	var receive socket.UDPReceive
	for packets := 0; packets < 16 && totalRead < 1<<20; packets++ {
		n, err := socket.RecvUDP(conn.fd, buffer, &receive)
		if err != nil {
			if isWouldBlock(err) {
				return nil
			}
			return err
		}
		totalRead += n
		conn.handleUDPPacket(buffer[:n], receive.Addr)
		if conn.isClosing() {
			return nil
		}
	}
	return nil
}

func (conn *fdConn) handleUDPPacket(packet []byte, source socket.UDPAddress) {
	packetConn := conn
	if conn.udp.peers != nil {
		// UDP children are logical connections that share the server fd.
		packetConn = conn.udp.peers[source]
		if packetConn == nil {
			sockaddr := source.Sockaddr()
			if sockaddr == nil {
				return
			}
			packetConn = &fdConn{fd: conn.fd, udp: &unixUDPState{remote: sockaddr, server: conn, key: source}}
			packetConn.events, packetConn.loop = conn.events, conn.loop
			packetConn.localAddr, packetConn.remoteAddr = conn.localAddr, source.NetAddr()
			conn.udp.peers[source] = packetConn
			packetConn.fireOnOpen()
		}
	}
	if packetConn.isClosing() {
		return
	}
	packetConn.inboundTail = packet
	packetConn.events.onSocketBytesRead(packetConn, len(packet))
	err := packetConn.fireOnData()
	_, _ = packetConn.Discard(-1)
	packetConn.inboundTail = nil
	if err != nil {
		packetConn.requestClose(err)
	}
}

func isWouldBlock(err error) bool {
	return err == syscall.EAGAIN || err == syscall.EWOULDBLOCK
}

func (conn *fdConn) runWakeTask() error {
	if conn.closed || conn.isClosing() {
		return net.ErrClosed
	}
	return conn.fireOnData()
}

func (conn *fdConn) Wake() error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	t := acquireTask(wakeTask, conn)
	if !conn.loop.submitTask(t) {
		releaseTask(t)
		return net.ErrClosed
	}
	return nil
}

func (conn *fdConn) Close() error { return conn.CloseWith(io.ErrUnexpectedEOF) }

func (conn *fdConn) CloseWith(err error) error {
	t := acquireTask(closeTask, conn)
	t.err = err
	// Setting closing and linking closeTask under submitMu puts Close after
	// external writes that have already reached their submission point.
	conn.submitMu.Lock()
	if conn.closing.Load() || conn.events.closing.Load() || conn.loop.stopping.Load() {
		conn.submitMu.Unlock()
		releaseTask(t)
		return net.ErrClosed
	}
	conn.closing.Store(true)
	if !conn.loop.pushTask(t) {
		conn.submitMu.Unlock()
		releaseTask(t)
		return net.ErrClosed
	}
	conn.submitMu.Unlock()
	conn.loop.notify()
	return nil
}

func (conn *fdConn) requestClose(err error) {
	if conn.isClosing() {
		return
	}
	t := acquireTask(closeTask, conn)
	t.err = err
	conn.submitMu.Lock()
	if conn.closing.Load() {
		conn.submitMu.Unlock()
		releaseTask(t)
		return
	}
	conn.closing.Store(true)
	if !conn.loop.pushTask(t) {
		// The queue is already stopping; preserve the I/O cause for shutdown.
		conn.deferredCloseErr = &err
		conn.submitMu.Unlock()
		releaseTask(t)
		return
	}
	conn.submitMu.Unlock()
	conn.loop.notify()
}

func (conn *fdConn) closeOnLoop(cause error) {
	if conn.closed {
		return
	}
	conn.closed = true
	conn.closing.Store(true)
	// Close gets one bounded flush attempt; a slow peer cannot delay shutdown.
	var flushErr error
	if !conn.isDatagram() && !conn.writeFailed {
		conn.writeBlocked = false
		_, flushErr = conn.flushOnLoop()
	}
	remaining := conn.pending.Load()
	var deferredCloseErr error
	if conn.deferredCloseErr != nil {
		deferredCloseErr = *conn.deferredCloseErr
		conn.deferredCloseErr = nil
	}
	finalErr := errors.Join(cause, deferredCloseErr, flushErr)
	if remaining > 0 {
		finalErr = errors.Join(finalErr, UnflushedError{Remaining: remaining})
	}
	conn.stopDeadlines()
	if conn.udp != nil && conn.udp.server != nil {
		// A child only leaves the peer map; its server owns the shared fd.
		delete(conn.udp.server.udp.peers, conn.udp.key)
	} else {
		if conn.udp != nil && conn.udp.peers != nil {
			children := conn.udp.peers
			conn.udp.peers = nil
			for _, child := range children {
				child.closeOnLoop(finalErr)
			}
		}
		conn.loop.delConn(conn)
		if conn.udp != nil && conn.udp.file != nil {
			_ = conn.udp.file.Close()
		} else {
			_ = syscall.Close(conn.fd)
		}
	}
	conn.outbound.Reset()
	conn.inbound.Reset()
	conn.inboundTail = nil
	conn.pending.Store(0)
	if callback := conn.events.OnClose; callback != nil && !conn.internal {
		callback(conn, finalErr)
	}
}

func (conn *fdConn) SetDeadline(deadline time.Time) error {
	return conn.setDeadline(deadlineBoth, deadline)
}
func (conn *fdConn) SetReadDeadline(deadline time.Time) error {
	return conn.setDeadline(deadlineRead, deadline)
}
func (conn *fdConn) SetWriteDeadline(deadline time.Time) error {
	return conn.setDeadline(deadlineWrite, deadline)
}

func (conn *fdConn) setDeadline(kind deadlineKind, deadline time.Time) error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.loop.inLoop() {
		return conn.applyDeadline(kind, deadline)
	}
	// Deadline setters are synchronous even though application happens on-loop.
	t := acquireTask(deadlineTask, conn)
	t.deadlineKind, t.deadline = kind, deadline
	t.done = make(chan error, 1)
	done := t.done
	if !conn.loop.submitTask(t) {
		releaseTask(t)
		return net.ErrClosed
	}
	return <-done
}

func (conn *fdConn) applyDeadline(kind deadlineKind, deadline time.Time) error {
	if conn.closed {
		return net.ErrClosed
	}
	state := conn.deadlines
	if state == nil {
		if deadline.IsZero() {
			return nil
		}
		state = &deadlineState{}
		conn.deadlines = state
	}
	// Generations make callbacks from a stopped or reset timer harmless.
	if kind == deadlineBoth || kind == deadlineRead {
		state.readGeneration++
		state.readDeadline = deadline
		state.readTimerGen.Store(state.readGeneration)
		state.readTimer = conn.resetDeadlineTimer(state.readTimer, deadlineRead, deadline)
	}
	if kind == deadlineBoth || kind == deadlineWrite {
		state.writeGeneration++
		state.writeDeadline = deadline
		state.writeTimerGen.Store(state.writeGeneration)
		state.writeTimer = conn.resetDeadlineTimer(state.writeTimer, deadlineWrite, deadline)
	}
	return nil
}

func (conn *fdConn) resetDeadlineTimer(timer *time.Timer, kind deadlineKind, deadline time.Time) *time.Timer {
	if timer != nil {
		timer.Stop()
	}
	if deadline.IsZero() {
		return timer
	}
	delay := time.Until(deadline)
	if delay < 0 {
		delay = 0
	}
	if timer == nil {
		// The callback only submits work; it never touches loop-owned state.
		return time.AfterFunc(delay, func() { conn.submitTimeout(kind) })
	}
	timer.Reset(delay)
	return timer
}

func (conn *fdConn) submitTimeout(kind deadlineKind) {
	state := conn.deadlines
	if conn.isClosing() || state == nil {
		return
	}
	t := acquireTask(timeoutTask, conn)
	t.deadlineKind = kind
	if kind == deadlineRead {
		t.generation = state.readTimerGen.Load()
	} else {
		t.generation = state.writeTimerGen.Load()
	}
	if !conn.loop.submitTask(t) {
		releaseTask(t)
	}
}

func (conn *fdConn) handleTimeout(kind deadlineKind, generation uint64) {
	state := conn.deadlines
	if conn.closed || conn.isClosing() || state == nil {
		return
	}
	var current uint64
	var deadline time.Time
	if kind == deadlineRead {
		current, deadline = state.readGeneration, state.readDeadline
	} else {
		current, deadline = state.writeGeneration, state.writeDeadline
	}
	// Reset races can submit the current generation early, so check time too.
	if generation != current || deadline.IsZero() || time.Now().Before(deadline) {
		return
	}
	conn.requestClose(fmt.Errorf("uio: %s deadline: %w", deadlineName(kind), os.ErrDeadlineExceeded))
}

func deadlineName(kind deadlineKind) string {
	if kind == deadlineRead {
		return "read"
	}
	return "write"
}

func (conn *fdConn) stopDeadlines() {
	state := conn.deadlines
	if state == nil {
		return
	}
	if state.readTimer != nil {
		state.readTimer.Stop()
	}
	if state.writeTimer != nil {
		state.writeTimer.Stop()
	}
}
