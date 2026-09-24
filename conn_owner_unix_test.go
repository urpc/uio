//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnectedUDPCallbacksStayOnOwningLoop(t *testing.T) {
	remote, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	openStarted := make(chan *net.UDPAddr, 1)
	releaseOpen := make(chan struct{})
	dataStarted := make(chan struct{})
	releaseData := make(chan struct{})
	secondData := make(chan struct{})
	var openOnce, dataOnce sync.Once
	releaseOpenFn := func() { openOnce.Do(func() { close(releaseOpen) }) }
	releaseDataFn := func() { dataOnce.Do(func() { close(releaseData) }) }
	defer releaseOpenFn()
	defer releaseDataFn()
	var callbacks atomic.Int32
	started := make(chan struct{}, 1)
	events := &Events{Pollers: 1}
	events.OnStart = func(*Events) { started <- struct{}{} }
	events.OnOpen = func(conn Conn) {
		openStarted <- conn.LocalAddr().(*net.UDPAddr)
		<-releaseOpen
	}
	events.OnData = func(conn Conn) error {
		switch callbacks.Add(1) {
		case 1:
			close(dataStarted)
			<-releaseData
		case 2:
			close(secondData)
		}
		_, _ = conn.Discard(-1)
		return nil
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	t.Cleanup(func() {
		releaseOpenFn()
		releaseDataFn()
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
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Events did not start")
	}
	type dialResult struct {
		conn Conn
		err  error
	}
	dialed := make(chan dialResult, 1)
	go func() {
		conn, err := events.DialContext(context.Background(), "udp://"+remote.LocalAddr().String(), nil)
		dialed <- dialResult{conn: conn, err: err}
	}()
	var local *net.UDPAddr
	select {
	case local = <-openStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("UDP OnOpen did not start")
	}
	if _, err := remote.WriteToUDP([]byte("packet"), local); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dataStarted:
		t.Fatal("UDP OnData overlapped OnOpen")
	case <-time.After(50 * time.Millisecond):
	}
	releaseOpenFn()
	var conn Conn
	select {
	case result := <-dialed:
		if result.err != nil {
			t.Fatal(result.err)
		}
		conn = result.conn
	case <-time.After(5 * time.Second):
		t.Fatal("UDP dial did not complete")
	}
	defer conn.Close()
	select {
	case <-dataStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("UDP OnData did not start")
	}
	if err := conn.Wake(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondData:
		t.Fatal("UDP Wake overlapped a packet callback")
	case <-time.After(50 * time.Millisecond):
	}
	releaseDataFn()
	select {
	case <-secondData:
	case <-time.After(5 * time.Second):
		t.Fatal("UDP Wake callback was not delivered")
	}
}

func TestUDPCallbackQueuesWriteToSameLoopStream(t *testing.T) {
	udpServer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udpServer.Close()

	var target atomic.Pointer[fdConn]
	var releaseOnce sync.Once
	releaseTCP := make(chan struct{})
	release := func() { releaseOnce.Do(func() { close(releaseTCP) }) }
	defer release()

	started := make(chan string, 1)
	tcpOpened := make(chan *fdConn, 1)
	udpOpened := make(chan struct{}, 1)
	tcpCallback := make(chan struct{}, 1)
	udpWrite := make(chan error, 1)
	events := &Events{Pollers: 1}
	events.OnStart = func(events *Events) {
		for _, listener := range events.acceptor.listeners {
			started <- listener.laddr.String()
			return
		}
	}
	events.OnOpen = func(conn Conn) {
		fdc := conn.(*fdConn)
		if fdc.isDatagram() {
			udpOpened <- struct{}{}
		} else {
			tcpOpened <- fdc
		}
	}
	events.OnData = func(conn Conn) error {
		if conn.(*fdConn).isDatagram() {
			_, err := target.Load().Write([]byte("cross"))
			udpWrite <- err
			_, _ = conn.Discard(-1)
			return nil
		}
		tcpCallback <- struct{}{}
		<-releaseTCP
		_, _ = conn.Discard(-1)
		return nil
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		release()
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

	var tcpAddr string
	select {
	case tcpAddr = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not start")
	}
	tcpClient, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpClient.Close()
	var tcpConn *fdConn
	select {
	case tcpConn = <-tcpOpened:
	case <-time.After(5 * time.Second):
		t.Fatal("TCP OnOpen was not called")
	}
	target.Store(tcpConn)

	udpConn, err := events.Dial("udp://"+udpServer.LocalAddr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer udpConn.Close()
	select {
	case <-udpOpened:
	case <-time.After(5 * time.Second):
		t.Fatal("UDP OnOpen was not called")
	}
	if _, err = tcpClient.Write([]byte("start")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tcpCallback:
	case <-time.After(5 * time.Second):
		t.Fatal("TCP task did not enter OnData")
	}
	if _, err = udpServer.WriteToUDP([]byte("trigger"), udpConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-udpWrite:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UDP callback did not complete its cross-connection write")
	}
	if tcpConn.pendingEvents.Load()&ioEventWrite == 0 {
		t.Fatal("UDP callback wrote directly to a stream owned by another task")
	}
	release()
	if err = tcpClient.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var response [5]byte
	if _, err = io.ReadFull(tcpClient, response[:]); err != nil || string(response[:]) != "cross" {
		t.Fatalf("TCP response = %q, %v", response, err)
	}
}
