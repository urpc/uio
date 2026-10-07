//go:build (darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import "net"

// inheritAcceptedOptions reports that this platform does not copy the
// listening socket's options to accepted sockets, so each accepted connection
// applies its own; the open turn does so through applyAcceptedOptions.
const inheritAcceptedOptions = false

// setListenerOptions has nothing to arm here.
func setListenerOptions(net.Listener) {}
