//go:build !windows

package uio

import (
	"errors"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func isPeerGoneWrite(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func setTTL(fd uintptr) error {
	return unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TTL, 3)
}

func mustSocket(t *testing.T, conn Conn) SocketConn {
	t.Helper()
	sc, ok := SocketOf(conn)
	if !ok {
		t.Fatal("SocketOf reported false for a socket connection")
	}
	return sc
}

func ttlOf(t *testing.T, conn Conn) int {
	t.Helper()
	var ttl int
	if err := mustSocket(t, conn).Control(func(fd uintptr) {
		value, err := unix.GetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TTL)
		if err != nil {
			t.Error(err)
			return
		}
		ttl = value
	}); err != nil {
		t.Fatalf("Control: %v", err)
	}
	return ttl
}

func TestDialWithLocalAddr(t *testing.T) {
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{})
	addr, _ := serveLoopback(t, events)
	conn, err := events.Dial("tcp://"+addr, WithLocalAddr("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok || !local.IP.Equal(net.IPv4(127, 0, 0, 1)) || local.Port == 0 {
		t.Fatalf("local address = %v, want 127.0.0.1 with a port", conn.LocalAddr())
	}
}

func TestDialWithControl(t *testing.T) {
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{})
	addr, _ := serveLoopback(t, events)
	var gotNetwork, gotAddr string
	conn, err := events.Dial("tcp://"+addr, WithDialControl(func(network, address string, fd uintptr) error {
		gotNetwork, gotAddr = network, address
		return setTTL(fd)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if gotNetwork != "tcp4" || gotAddr != addr {
		t.Fatalf("control saw %s %s, want tcp4 %s", gotNetwork, gotAddr, addr)
	}
	if ttl := ttlOf(t, conn); ttl != 3 {
		t.Fatalf("IP_TTL = %d, want the one the control set", ttl)
	}
}

func TestDialWithControlErrorFails(t *testing.T) {
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{})
	addr, _ := serveLoopback(t, events)
	want := errors.New("no")
	if _, err := events.Dial("tcp://"+addr, WithDialControl(func(string, string, uintptr) error {
		return want
	})); !errors.Is(err, want) {
		t.Fatalf("Dial = %v, want the control's error", err)
	}
}

func TestListenWithControl(t *testing.T) {
	for _, reusePort := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "reuseport"}[reusePort], func(t *testing.T) {
			events := withFuncs(&Events{Pollers: 1}, FuncHandler{})
			events.ReusePort = reusePort
			serveLoopback(t, events)
			var runs atomic.Int32
			ln, err := events.Listen("tcp://127.0.0.1:0", WithListenControl(func(network, address string, fd uintptr) error {
				if network != "tcp4" {
					t.Errorf("control network = %s, want tcp4", network)
				}
				kind, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TYPE)
				if err != nil {
					return err
				}
				if kind != unix.SOCK_STREAM {
					t.Errorf("SO_TYPE = %d, want a stream socket", kind)
				}
				runs.Add(1)
				return setTTL(fd)
			}))
			if err != nil {
				t.Fatal(err)
			}
			if runs.Load() == 0 {
				t.Fatal("the control never ran")
			}
			client, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_ = client.Close()
		})
	}
}

func TestDialWithLocalAddrUnixSchemeStripped(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "u")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	remote := filepath.Join(dir, "t.sock")
	local := filepath.Join(dir, "s.sock")
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{})
	done := make(chan error, 1)
	go func() {
		done <- events.Serve("unix://" + remote)
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	for !events.ready.Load() {
		select {
		case err := <-done:
			t.Fatalf("Serve returned: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	conn, err := events.Dial("unix://"+remote, WithLocalAddr("unix://"+local))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := conn.LocalAddr().String(); got != local {
		t.Fatalf("local address = %q, want %q", got, local)
	}
}
