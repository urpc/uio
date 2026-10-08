//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

package uio

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUnixConnectionAddressesAreKept pins that non-IP peers keep their address
// objects. Unix addresses have no value form, and dropping them made
// RemoteAddr return nil, which panicked callers that use it.
func TestUnixConnectionAddressesAreKept(t *testing.T) {
	// The socket path is limited to about 100 bytes, shorter than a test
	// TempDir on darwin.
	dir, err := os.MkdirTemp("/tmp", "uio-unix-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// Dial side: the uio connection must report the listener's Unix path.
	dialPath := filepath.Join(dir, "peer.sock")
	listener, err := net.Listen("unix", dialPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peerAccepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			peerAccepted <- conn
		}
	}()

	clientEvents := &Events{Pollers: 1}
	clientStarted := make(chan struct{}, 1)
	clientEvents.OnStart = func(*Events) { clientStarted <- struct{}{} }
	clientDone := make(chan error, 1)
	go func() { clientDone <- clientEvents.Serve() }()
	t.Cleanup(func() {
		_ = clientEvents.Close(nil)
		<-clientDone
	})
	<-clientStarted
	dialed, err := clientEvents.Dial("unix://"+dialPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	peer := <-peerAccepted
	defer peer.Close()
	if addr := dialed.RemoteAddr(); addr == nil || addr.Network() != "unix" || addr.String() != dialPath {
		t.Fatalf("dialed RemoteAddr = %v (%T), want unix %s", addr, addr, dialPath)
	}
	_ = dialed.Close()

	// Accept side: a uio Unix listener must report its peer and its own path.
	servePath := filepath.Join(dir, "server.sock")
	serverEvents := &Events{Pollers: 1}
	serverStarted := make(chan struct{}, 1)
	serverEvents.OnStart = func(*Events) { serverStarted <- struct{}{} }
	accepted := make(chan Conn, 1)
	serverEvents.OnOpen = func(conn Conn) { accepted <- conn }
	serverDone := make(chan error, 1)
	go func() { serverDone <- serverEvents.Serve("unix://" + servePath) }()
	t.Cleanup(func() {
		_ = serverEvents.Close(nil)
		<-serverDone
	})
	<-serverStarted

	var peerConn net.Conn
	for deadline := time.Now().Add(5 * time.Second); peerConn == nil && time.Now().Before(deadline); {
		peerConn, err = net.Dial("unix", servePath)
		if err != nil {
			time.Sleep(time.Millisecond)
		}
	}
	if peerConn == nil {
		t.Fatal(err)
	}
	defer peerConn.Close()
	serverConn := <-accepted
	if addr := serverConn.RemoteAddr(); addr == nil || addr.Network() != "unix" {
		t.Fatalf("accepted RemoteAddr = %v (%T), want a unix address", addr, addr)
	}
	if addr := serverConn.LocalAddr(); addr == nil || addr.String() != servePath {
		t.Fatalf("accepted LocalAddr = %v, want %s", addr, servePath)
	}
}
