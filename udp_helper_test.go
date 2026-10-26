//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly || stdio

package uio

import (
	"net"
	"testing"
)

// listenUDPLoopback binds a throwaway UDP socket as a packet test's far end.
func listenUDPLoopback(t *testing.T) (net.PacketConn, error) {
	t.Helper()
	return net.ListenPacket("udp", "127.0.0.1:0")
}
