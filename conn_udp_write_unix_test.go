//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestExternalUDPServerPeerWrites(t *testing.T) {
	started := make(chan string, 1)
	peer := make(chan Conn, 1)
	closed := make(chan struct{}, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: 8}
	events.OnStart = func(ev *Events) {
		ev.acceptor.mux.Lock()
		for _, listener := range ev.acceptor.listeners {
			started <- listener.laddr.String()
			break
		}
		ev.acceptor.mux.Unlock()
	}
	events.OnData = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		select {
		case peer <- conn:
		default:
		}
		return nil
	}
	events.OnClose = func(Conn, error) { closed <- struct{}{} }
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("udp://127.0.0.1:0") }()
	defer func() {
		_ = events.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Error("UDP server did not stop")
		}
	}()

	var address string
	select {
	case address = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("UDP server did not start")
	}
	remote, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Write([]byte("open")); err != nil {
		t.Fatal(err)
	}
	var conn Conn
	select {
	case conn = <-peer:
	case <-time.After(3 * time.Second):
		t.Fatal("UDP peer callback did not run")
	}

	for _, payload := range []string{"plain", "owned"} {
		var n int
		if payload == "plain" {
			n, err = conn.Write([]byte(payload))
		} else {
			buffer := AcquireBuffer(len(payload))
			_, _ = buffer.WriteString(payload)
			n, err = conn.WriteOwned(buffer)
		}
		if n != len(payload) || err != nil {
			t.Fatalf("external UDP write %q = %d, %v", payload, n, err)
		}
		if err = client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var received [16]byte
		read, readErr := client.Read(received[:])
		if readErr != nil || string(received[:read]) != payload {
			t.Fatalf("UDP datagram = %q, %v; want %q", received[:read], readErr, payload)
		}
	}
	if n, err := conn.Write([]byte("too-large")); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("UDP overflow = %d, %v", n, err)
	}
	if n, err := conn.Writev([][]byte{[]byte("x")}); n != 0 || !errors.Is(err, errUnsupported) {
		t.Fatalf("UDP Writev = %d, %v", n, err)
	}
	if got := conn.OutboundBuffered(); got != 0 {
		t.Fatalf("pending UDP bytes = %d", got)
	}
	if err := conn.CloseWith(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("UDP peer close callback did not run")
	}
	if n, err := conn.Write([]byte("late")); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after close = %d, %v", n, err)
	}
}

func TestExternalConnectedUDPWrites(t *testing.T) {
	receiver, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	started := make(chan struct{}, 1)
	events := &Events{Pollers: 1, OnStart: func(*Events) { started <- struct{}{} }}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	defer func() {
		_ = events.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Error("dial-only Events did not stop")
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("dial-only Events did not start")
	}
	conn, err := events.DialContext(t.Context(), "udp://"+receiver.LocalAddr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{"first", "second"} {
		var n int
		if payload == "first" {
			n, err = conn.Write([]byte(payload))
		} else {
			buffer := AcquireBuffer(len(payload))
			_, _ = buffer.WriteString(payload)
			n, err = conn.WriteOwned(buffer)
		}
		if n != len(payload) || err != nil {
			t.Fatalf("connected UDP write %q = %d, %v", payload, n, err)
		}
		if err = receiver.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var received [16]byte
		read, _, readErr := receiver.ReadFromUDP(received[:])
		if readErr != nil || string(received[:read]) != payload {
			t.Fatalf("connected UDP datagram = %q, %v; want %q", received[:read], readErr, payload)
		}
	}
}

func TestUDPWriteRejectsAnotherEventsLoop(t *testing.T) {
	events := &Events{}
	current := &eventLoop{}
	owner := currentGoroutineID()
	current.loopGoid.Store(owner)
	activeEventLoops.Store(owner, struct{}{})
	defer activeEventLoops.Delete(owner)
	target := &eventLoop{}
	events.workers = []*eventLoop{target}
	conn := &fdConn{udp: &unixUDPState{}}
	conn.events, conn.loop = events, target
	if n, err := conn.Write([]byte("x")); n != 0 || !errors.Is(err, ErrUDPWriteOnEventLoop) {
		t.Fatalf("cross-loop UDP Write = %d, %v", n, err)
	}
	owned := AcquireBuffer(1)
	_, _ = owned.WriteString("x")
	if n, err := conn.WriteOwned(owned); n != 0 || !errors.Is(err, ErrUDPWriteOnEventLoop) {
		t.Fatalf("cross-loop UDP WriteOwned = %d, %v", n, err)
	}
	if got := conn.OutboundBuffered(); got != 0 {
		t.Fatalf("cross-loop UDP pending bytes = %d", got)
	}
}

func TestExternalUDPWouldBlockPreservesConnection(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	payload := fillDatagramSendBuffer(t, fds[0])
	events := &Events{MaxBufferSize: 64}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	events.master = loop
	serveDone := make(chan error, 1)
	go func() { serveDone <- loop.Serve(false, nil) }()
	defer func() {
		loop.beginStop(nil)
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Error("UDP test loop did not stop")
		}
	}()
	conn := &fdConn{fd: fds[0], udp: &unixUDPState{}}
	conn.events, conn.loop = events, loop
	if n, err := conn.Write(payload); n != 0 || !isUDPSendBlocked(err) {
		t.Fatalf("blocked external UDP write = %d, %v", n, err)
	}
	if conn.isClosing() || conn.OutboundBuffered() != 0 {
		t.Fatalf("blocked UDP write closed or retained bytes: closing=%v pending=%d", conn.isClosing(), conn.OutboundBuffered())
	}
}

func TestQueuedUDPWriteAndCloseOrdering(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		name := "write_before_close"
		if closeFirst {
			name = "released_before_write_task"
		}
		t.Run(name, func(t *testing.T) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fds[1])
			events := &Events{MaxBufferSize: 64}
			loop, err := newEventLoop(events)
			if err != nil {
				_ = unix.Close(fds[0])
				t.Fatal(err)
			}
			defer func() {
				_ = loop.poller.Close(nil)
				loop.ioPool.stop()
			}()
			events.master = loop
			conn := &fdConn{fd: fds[0], udp: &unixUDPState{}}
			conn.events, conn.loop = events, loop
			defer func() {
				if !conn.isClosedOnLoop() {
					_ = unix.Close(fds[0])
				}
			}()

			result := make(chan udpWriteResult, 1)
			go func() {
				n, writeErr := conn.Write([]byte("packet"))
				result <- udpWriteResult{n: n, err: writeErr}
			}()
			deadline := time.Now().Add(3 * time.Second)
			for conn.OutboundBuffered() != len("packet") {
				if time.Now().After(deadline) {
					t.Fatal("external UDP write was not queued")
				}
				time.Sleep(time.Millisecond)
			}
			if closeFirst {
				conn.closeOnLoop(nil) // shutdown can release before a queued task runs.
			} else if err := conn.CloseWith(nil); err != nil {
				t.Fatal(err)
			}
			loop.runTasks(2)
			select {
			case got := <-result:
				if closeFirst {
					if got.n != 0 || !errors.Is(got.err, net.ErrClosed) {
						t.Fatalf("write after release = %d, %v", got.n, got.err)
					}
				} else if got.n != len("packet") || got.err != nil {
					t.Fatalf("write before close = %d, %v", got.n, got.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("queued UDP writer did not complete")
			}
			if got := conn.OutboundBuffered(); got != 0 {
				t.Fatalf("pending bytes after close = %d", got)
			}
			if !closeFirst {
				if got := string(readPeer(t, fds[1], len("packet"))); got != "packet" {
					t.Fatalf("datagram before close = %q", got)
				}
			}
		})
	}
}
