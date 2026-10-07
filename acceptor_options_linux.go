//go:build linux && !stdio

package uio

import (
	"net"

	"golang.org/x/sys/unix"
)

// inheritAcceptedOptions reports that Linux copies the listening socket's TCP
// options to every socket it accepts, so an accepted connection needs no
// setsockopt of its own; see setListenerOptions.
const inheritAcceptedOptions = true

// setListenerOptions arms a listening socket with the options every stream
// connection wants. Linux copies these — and the keepalive timers — from the
// listener to each accepted socket, so accepting a connection costs no
// setsockopt at all. A connection may still override any of them through the
// per-connection setters afterwards.
func setListenerOptions(ln net.Listener) {
	tcp, ok := ln.(*net.TCPListener)
	if !ok {
		return
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1)
		_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_KEEPIDLE, defaultTCPKeepAliveSecs)
	})
}
