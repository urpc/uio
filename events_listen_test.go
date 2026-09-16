package uio

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestServeInitializesListeners(t *testing.T) {
	for _, test := range []struct {
		name  string
		addrs []string
		want  int
	}{
		{name: "dial only", want: 0},
		{name: "empty address", addrs: []string{""}, want: 0},
		{name: "listener", addrs: []string{"tcp://127.0.0.1:0"}, want: 1},
		{name: "listeners", addrs: []string{"tcp://127.0.0.1:0", "tcp://127.0.0.1:0"}, want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &Events{Pollers: 1}
			events.OnStart = func(events *Events) {
				if got := len(events.acceptor.listeners); got != test.want {
					t.Errorf("listeners = %d, want %d", got, test.want)
				}
				_ = events.Close(nil)
			}
			if err := events.Serve(test.addrs...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServeAcceptsConnectionsOnAllAddresses(t *testing.T) {
	events := &Events{Pollers: 1}
	started := make(chan []string, 1)
	events.OnStart = func(events *Events) {
		addrs := make([]string, 0, len(events.acceptor.listeners))
		for _, listener := range events.acceptor.listeners {
			addrs = append(addrs, listener.ln.Addr().String())
		}
		started <- addrs
	}
	events.OnData = func(conn Conn) error {
		_, err := conn.WriteTo(conn)
		return err
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0", "tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case err := <-serveDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve did not stop")
		}
	})

	var addrs []string
	select {
	case addrs = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not start")
	}
	if len(addrs) != 2 || addrs[0] == addrs[1] {
		t.Fatalf("listening addresses = %v, want two distinct addresses", addrs)
	}
	for _, addr := range addrs {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err = conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if _, err = conn.Write([]byte("ping")); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		var response [4]byte
		_, err = io.ReadFull(conn, response[:])
		conn.Close()
		if err != nil || string(response[:]) != "ping" {
			t.Fatalf("echo on %s = %q, %v", addr, response, err)
		}
	}
}

func TestServeRollsBackListenersWhenLaterAddressFails(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	if err = reserved.Close(); err != nil {
		t.Fatal(err)
	}

	events := &Events{Pollers: 1}
	if err = events.Serve(addr, "unsupported://address"); err == nil {
		t.Fatal("Serve accepted an unsupported second address")
	}
	if events.ready.Load() || !events.closing.Load() {
		t.Fatal("failed Serve left event loops serving")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("first listener was not released: %v", err)
	}
	_ = listener.Close()
}
