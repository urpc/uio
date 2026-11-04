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

// Socket options and descriptor ownership: setters run on the turn or at
// accept, a Control standing on the descriptor, and the gate keeping a
// server socket open while a syscall uses it.

package uio

import (
	"net"
	"syscall"
	"time"

	"github.com/urpc/uio/internal/socket"
)

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

// setSocketOption applies an option on the caller's goroutine. The
// connection's own turn releases the descriptor, so it applies options
// directly; any other caller holds the owner's submitMu across the syscall,
// which teardown takes to mark the connection released before it closes the
// descriptor, so the option reaches this connection's socket or nothing.
func (conn *fdConn) setSocketOption(kind socketOptionKind, value int) error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.directOwner() {
		return conn.applySocketOption(kind, value)
	}
	owner := conn.descriptorOwner()
	owner.submitMu.Lock()
	defer owner.submitMu.Unlock()
	return conn.applySocketOption(kind, value)
}

// Control runs fn with the connection's descriptor, for the socket options
// the dedicated setters do not carry; see Conn.Control. A UDP child's options
// land on the socket it shares with its server.
func (conn *fdConn) Control(fn func(fd uintptr)) error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.directOwner() {
		return conn.applyControl(fn)
	}
	owner := conn.descriptorOwner()
	if owner.loop == nil || !owner.loop.acquireIO() {
		return net.ErrClosed
	}
	defer owner.loop.releaseIO()
	owner.submitMu.Lock()
	if conn.isClosing() || owner.isClosing() {
		owner.submitMu.Unlock()
		return net.ErrClosed
	}
	// Keep the socket alive without holding a submission lock across user
	// code: fn may close this connection or its listener. A temporary alias
	// applies the same socket options and cannot be reused under the call.
	gate, open := conn.enterDescriptor()
	if !open {
		owner.submitMu.Unlock()
		return net.ErrClosed
	}
	fd, err := socket.Dup(int(owner.fd))
	gate.leave()
	if err == nil {
		owner.flags.Or(controlAliasFlag)
	}
	owner.submitMu.Unlock()
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	fn(uintptr(fd))
	return nil
}

// applyControl runs fn on the turn that owns the descriptor. On a UDP
// server's socket the gate stays held across fn: a Listener.Close elsewhere
// waits for fn to return before closing the socket, and one from fn itself
// leaves the close to this turn.
func (conn *fdConn) applyControl(fn func(fd uintptr)) error {
	if conn.close.isReleased() || conn.descriptorOwner().close.isReleased() {
		return net.ErrClosed
	}
	gate, open := conn.enterDescriptor()
	if !open {
		return net.ErrClosed
	}
	defer gate.leave()
	conn.descriptorOwner().flags.And(^offTurnCloseFlag)
	fn(uintptr(conn.fd))
	return nil
}

// enterDescriptor counts a syscall on a UDP server's socket, which children
// and Listener.Close reach from any goroutine, and reports false once that
// socket is closing; see descriptorGate. Leave the returned gate when done; it
// is nil for a connection whose descriptor is its own, which closes only in
// its own turn.
func (conn *fdConn) enterDescriptor() (*descriptorGate, bool) {
	owner := conn.descriptorOwner()
	if !owner.internal || owner.udp == nil {
		return nil, true
	}
	gate := &owner.udp.gate
	return gate, gate.enter()
}

// releaseSocket hands the socket close to its one owner, started by whoever
// calls first, and returns a channel closed once the socket is closed: it
// stops new syscalls on the socket, then — once those in progress have ended
// — takes the descriptor out of the fd table and the poller, explicitly since
// a Control alias may keep the socket open, and closes it. A Control that
// outlasts socketCloseWait holds the close, which keeps retrying, so waiting
// on the channel is waiting on a fn that may be waiting for the close itself:
// bound the wait, and let the retry close once the fn returns.
func (server *fdConn) releaseSocket() <-chan struct{} {
	server.udp.closeMu.Lock()
	if server.udp.closed != nil {
		done := server.udp.closed
		server.udp.closeMu.Unlock()
		return done
	}
	done := make(chan struct{})
	// Counted before it is published: whoever finds the close under way —
	// the shutdown's own teardown among them — finds Serve's wait covering it
	// too, so Serve returns with the address free.
	server.events().socketCloses.add()
	server.udp.closed = done
	server.udp.closeMu.Unlock()
	if hook := testHookSocketClosePublished.Load(); hook != nil {
		(*hook)(server)
	}
	go func() {
		defer server.events().socketCloses.done()
		defer close(done)
		server.udp.gate.markClosed()
		for !server.udp.gate.waitDrain(socketCloseWait) {
			time.Sleep(socketCloseWait)
		}
		server.loop.fdMap.DeleteValue(int(server.fd), server)
		_ = server.loop.poller.Remove(int(server.fd), server.currentInterest())
		if server.udp.file != nil {
			_ = server.udp.file.Close()
		} else {
			_ = syscall.Close(int(server.fd))
		}
	}()
	return done
}

// releaseSocketNow closes a closing UDP server's socket on the caller, so the
// address is free once Listener.Close returns — unless a Control on the
// socket outlasts the wait. The server's own turn — a callback of one of its
// peers — may hold the gate in a Control, so there the release at the end of
// the turn closes it instead.
func (server *fdConn) releaseSocketNow() {
	if server.ioOwner.Load() == currentGoroutineID() {
		return
	}
	select {
	case <-server.releaseSocket():
	case <-time.After(2 * socketCloseWait):
	}
}

// descriptorOwner is the connection whose release closes this connection's
// descriptor: a UDP child shares its server's.
func (conn *fdConn) descriptorOwner() *fdConn {
	if conn.udp != nil && conn.udp.server != nil {
		return conn.udp.server
	}
	return conn
}

func (conn *fdConn) applySocketOption(kind socketOptionKind, value int) error {
	if conn.close.isReleased() || conn.descriptorOwner().close.isReleased() {
		return net.ErrClosed
	}
	gate, open := conn.enterDescriptor()
	if !open {
		return net.ErrClosed
	}
	defer gate.leave()
	switch kind {
	case optionLinger:
		if value > 0 {
			conn.flags.And(^offTurnCloseFlag)
		}
		return socket.SetLinger(int(conn.fd), value)
	case optionNoDelay:
		return socket.SetNoDelay(int(conn.fd), value != 0)
	case optionKeepAlive:
		return socket.SetKeepAlive(int(conn.fd), value != 0)
	case optionKeepAlivePeriod:
		return socket.SetKeepAlivePeriod(int(conn.fd), value)
	case optionReadBuffer:
		return socket.SetRecvBuffer(int(conn.fd), value)
	default:
		return socket.SetSendBuffer(int(conn.fd), value)
	}
}
