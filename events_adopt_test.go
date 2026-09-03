package uio

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestAdoptTransfersTCPConnection(t *testing.T) {
	events := &Events{Pollers: 1}
	started := make(chan struct{})
	opened := make(chan Conn, 1)
	received := make(chan string, 1)
	events.OnStart = func(*Events) { close(started) }
	events.OnOpen = func(conn Conn) { opened <- conn }
	events.OnData = func(conn Conn) error {
		payload := make([]byte, conn.InboundBuffered())
		_, err := conn.Read(payload)
		if err == nil {
			received <- string(payload)
		}
		return err
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("Events.Serve did not stop")
		}
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Events.Serve did not start")
	}

	client, server := tcpConnectionPair(t)
	defer client.Close()
	const userdata = "adopted"
	adopted, err := events.Adopt(server, userdata)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case conn := <-opened:
		if conn != adopted || conn.Userdata() != userdata {
			t.Fatalf("OnOpen connection = %v/%v, want adopted userdata", conn, conn.Userdata())
		}
	case <-time.After(time.Second):
		t.Fatal("Adopt did not call OnOpen")
	}

	if _, err = client.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-received:
		if payload != "payload" {
			t.Fatalf("received payload = %q", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("adopted connection did not receive data")
	}
}

func TestAdoptClosesConnectionWhenEventsNotServing(t *testing.T) {
	client, server := tcpConnectionPair(t)
	defer client.Close()
	if conn, err := (&Events{}).Adopt(server, nil); conn != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Adopt() = %v, %v; want nil, net.ErrClosed", conn, err)
	}
	expectPeerClosed(t, client)
}

func tcpConnectionPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case server := <-accepted:
		return client, server
	case err := <-acceptErr:
		client.Close()
		t.Fatal(err)
	case <-time.After(time.Second):
		client.Close()
		t.Fatal("listener did not accept connection")
	}
	return nil, nil
}
