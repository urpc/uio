//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"net"
	"syscall"
	"testing"
	"time"
)

func TestAcceptBurstAcrossListeners(t *testing.T) {
	type acceptedOpen struct {
		addr               string
		noDelay, keepAlive int
		err                error
	}
	const extraConnections = 1
	want := acceptBatchSize + extraConnections + 1
	events := &Events{Pollers: 1}
	started := make(chan []string, 1)
	opened := make(chan acceptedOpen, want)
	events.OnStart = func(events *Events) {
		addrs := make([]string, 0, len(events.acceptor.listeners))
		for _, listener := range events.acceptor.listeners {
			addrs = append(addrs, listener.laddr.String())
		}
		started <- addrs
	}
	events.OnOpen = func(conn Conn) {
		fd := conn.(*fdConn).fd
		noDelay, err := syscall.GetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY)
		var keepAlive int
		if err == nil {
			keepAlive, err = syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
		}
		opened <- acceptedOpen{addr: conn.LocalAddr().String(), noDelay: noDelay, keepAlive: keepAlive, err: err}
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0", "tcp://127.0.0.1:0") }()
	var clients []net.Conn
	t.Cleanup(func() {
		for _, conn := range clients {
			_ = conn.Close()
		}
		_ = events.Close(nil)
		select {
		case err := <-serveDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})

	var addrs []string
	select {
	case addrs = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not start")
	}
	if len(addrs) != 2 || addrs[0] == addrs[1] {
		t.Fatalf("listening addresses = %v, want two distinct addresses", addrs)
	}

	for index := 0; index < want; index++ {
		addr := addrs[0]
		if index == want-1 {
			addr = addrs[1]
		}
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial %d to %s: %v", index, addr, err)
		}
		clients = append(clients, conn)
	}

	counts := make(map[string]int)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for index := 0; index < want; index++ {
		select {
		case result := <-opened:
			if result.err != nil || result.noDelay == 0 || result.keepAlive == 0 {
				t.Fatalf("OnOpen socket options = noDelay %d, keepAlive %d, error %v", result.noDelay, result.keepAlive, result.err)
			}
			counts[result.addr]++
		case <-deadline.C:
			t.Fatalf("OnOpen called %d/%d times; addresses = %v", index, want, counts)
		}
	}
	if counts[addrs[0]] != acceptBatchSize+extraConnections || counts[addrs[1]] != 1 {
		t.Fatalf("OnOpen counts = %v, want %d and 1", counts, acceptBatchSize+extraConnections)
	}
}
