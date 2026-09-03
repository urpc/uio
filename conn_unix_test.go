//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
	"golang.org/x/sys/unix"
)

type testConnection struct {
	conn *fdConn
	peer int
	stop func()
}

func newTestConnection(t *testing.T, events *Events) testConnection {
	t.Helper()
	testConn, registered := startTestConnection(t, events)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	return testConn
}

func startTestConnection(t *testing.T, events *Events) (testConnection, <-chan error) {
	t.Helper()
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	events.workers = []*eventLoop{loop}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	if err = unix.SetNonblock(fds[1], true); err != nil {
		t.Fatal(err)
	}
	conn := &fdConn{fd: fds[0]}
	conn.events = events
	conn.loop = loop

	done := make(chan error, 1)
	go func() { done <- loop.Serve(false, nil) }()
	registered := make(chan error, 1)
	go func() { registered <- events.addConn(conn) }()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			if !conn.isClosing() {
				_ = conn.CloseWith(io.EOF)
			}
			loop.beginStop(nil)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("event loop did not stop")
			}
			_ = unix.Close(fds[1])
		})
	}
	t.Cleanup(stop)
	return testConnection{conn: conn, peer: fds[1], stop: stop}, registered
}

func readPeer(t *testing.T, fd int, size int) []byte {
	t.Helper()
	result := make([]byte, size)
	offset := 0
	deadline := time.Now().Add(2 * time.Second)
	for offset < size && time.Now().Before(deadline) {
		n, err := unix.Read(fd, result[offset:])
		if n > 0 {
			offset += n
		}
		if err == nil {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			time.Sleep(time.Millisecond)
			continue
		}
		t.Fatal(err)
	}
	if offset != size {
		t.Fatalf("read %d bytes, want %d", offset, size)
	}
	return result
}

func TestCallbacksStayOnLoopAndCallbackCloseIsOrdered(t *testing.T) {
	var mu sync.Mutex
	var sequence []string
	var loopID int64
	closed := make(chan error, 1)
	cause := errors.New("callback close")
	events := &Events{Pollers: 1}
	events.OnOpen = func(Conn) {
		mu.Lock()
		loopID = currentGoroutineID()
		sequence = append(sequence, "open")
		mu.Unlock()
	}
	events.OnData = func(conn Conn) error {
		mu.Lock()
		if currentGoroutineID() != loopID {
			t.Errorf("OnData ran on a different goroutine")
		}
		sequence = append(sequence, "data")
		mu.Unlock()
		_, _ = conn.Discard(-1)
		return conn.CloseWith(cause)
	}
	events.OnClose = func(_ Conn, err error) {
		mu.Lock()
		if currentGoroutineID() != loopID {
			t.Errorf("OnClose ran on a different goroutine")
		}
		sequence = append(sequence, "close")
		mu.Unlock()
		closed <- err
	}
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("request")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, cause) {
			t.Fatalf("close error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnClose was not called")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"open", "data", "close"}
	if len(sequence) != len(want) {
		t.Fatalf("callback sequence = %v", sequence)
	}
	for i := range want {
		if sequence[i] != want[i] {
			t.Fatalf("callback sequence = %v", sequence)
		}
	}
}

func TestDialRejectedOnEventLoopCallback(t *testing.T) {
	dialResult := make(chan error, 1)
	events := &Events{Pollers: 1}
	events.OnData = func(conn Conn) error {
		started := time.Now()
		dialed, err := events.Dial("tcp://127.0.0.1:1", nil)
		if dialed != nil {
			_ = dialed.Close()
		}
		if !errors.Is(err, ErrDialOnEventLoop) {
			dialResult <- fmt.Errorf("callback Dial error = %v", err)
		} else if time.Since(started) > 100*time.Millisecond {
			dialResult <- fmt.Errorf("callback Dial returned too slowly")
		} else {
			dialResult <- nil
		}
		_, _ = conn.Discard(-1)
		_, err = conn.Write([]byte("ok"))
		return err
	}
	testConn := newTestConnection(t, events)
	events.ready.Store(true)
	if _, err := unix.Write(testConn.peer, []byte("dial")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-dialResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback Dial did not return")
	}
	if got := string(readPeer(t, testConn.peer, 2)); got != "ok" {
		t.Fatalf("same-loop response = %q", got)
	}
}

func TestExternalWriteCopiesAndWakePreservesOrder(t *testing.T) {
	woke := make(chan struct{}, 1)
	events := &Events{Pollers: 1}
	events.OnData = func(conn Conn) error {
		_, err := conn.WriteString("second")
		woke <- struct{}{}
		return err
	}
	testConn := newTestConnection(t, events)
	first := []byte("first-")
	if n, err := testConn.conn.Write(first); err != nil || n != len(first) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	copy(first, "xxxxxx")
	if err := testConn.conn.Wake(); err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, testConn.peer, len("first-second"))); got != "first-second" {
		t.Fatalf("peer received %q", got)
	}
	select {
	case <-woke:
	case <-time.After(time.Second):
		t.Fatal("Wake did not invoke OnData")
	}
}

func TestExternalWritevCopiesOnceWithoutMutatingCallerVector(t *testing.T) {
	events := &Events{Pollers: 1}
	testConn := newTestConnection(t, events)
	first := []byte("first-")
	second := []byte("second")
	vec := [][]byte{first, second}
	if n, err := testConn.conn.Writev(vec); err != nil || n != len("first-second") {
		t.Fatalf("Writev = %d, %v", n, err)
	}
	if len(vec) != 2 || len(vec[0]) != len("first-") || len(vec[1]) != len("second") {
		t.Fatalf("Writev mutated vector: %q", vec)
	}
	copy(first, "xxxxxx")
	copy(second, "yyyyyy")
	vec[0] = nil
	if got := string(readPeer(t, testConn.peer, len("first-second"))); got != "first-second" {
		t.Fatalf("peer received %q", got)
	}
}

func TestOutboundLimitsRejectWithoutChangingAcceptedBytes(t *testing.T) {
	opened := make(chan *fdConn, 1)
	releaseOpen := make(chan struct{})
	events := &Events{Pollers: 1, MaxOutboundBuffered: 4, MaxPendingWrites: 8}
	events.OnOpen = func(conn Conn) {
		opened <- conn.(*fdConn)
		<-releaseOpen
	}

	testConn, registered := startTestConnection(t, events)
	conn := <-opened
	data := []byte("1234")
	if n, err := conn.Write(data); err != nil || n != 4 {
		t.Fatalf("first Write = %d, %v", n, err)
	}
	if n, err := conn.Write([]byte("5")); !errors.Is(err, ErrOutboundOverflow) || n != 0 {
		t.Fatalf("overflow Write = %d, %v", n, err)
	}
	if got := conn.OutboundBuffered(); got != 4 {
		t.Fatalf("OutboundBuffered = %d", got)
	}
	copy(data, "xxxx")
	close(releaseOpen)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, testConn.peer, 4)); got != "1234" {
		t.Fatalf("peer received %q", got)
	}
}

func TestOutboundLimitDoesNotRejectImmediateLoopWrite(t *testing.T) {
	result := make(chan error, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: 4}
	events.OnOpen = func(conn Conn) {
		n, err := conn.Write([]byte("12345678"))
		if err == nil && n != 8 {
			err = fmt.Errorf("Write returned %d bytes", n)
		}
		result <- err
	}
	testConn := newTestConnection(t, events)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, testConn.peer, 8)); got != "12345678" {
		t.Fatalf("peer received %q", got)
	}
}

func TestPendingWriteTaskLimit(t *testing.T) {
	opened := make(chan *fdConn, 1)
	releaseOpen := make(chan struct{})
	events := &Events{Pollers: 1, MaxOutboundBuffered: 1024, MaxPendingWrites: 1}
	events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn); <-releaseOpen }
	testConn, registered := startTestConnection(t, events)
	conn := <-opened
	if _, err := conn.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}
	if n, err := conn.Write([]byte("two")); !errors.Is(err, ErrTaskQueueFull) || n != 0 {
		t.Fatalf("second Write = %d, %v", n, err)
	}
	close(releaseOpen)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, testConn.peer, 3)); got != "one" {
		t.Fatalf("peer received %q", got)
	}
}

func TestFlushTaskIsAWriteBarrier(t *testing.T) {
	opened := make(chan *fdConn, 1)
	releaseOpen := make(chan struct{})
	outbound := make(chan int, 4)
	events := &Events{Pollers: 1}
	events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn); <-releaseOpen }
	events.OnOutbound = func(_ Conn, written int) { outbound <- written }
	testConn, registered := startTestConnection(t, events)
	conn := <-opened
	if _, err := conn.Write([]byte("A")); err != nil {
		t.Fatal(err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("B")); err != nil {
		t.Fatal(err)
	}
	close(releaseOpen)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, testConn.peer, 2)); got != "AB" {
		t.Fatalf("peer received %q", got)
	}
	first, second := <-outbound, <-outbound
	if first != 1 || second != 1 {
		t.Fatalf("OnOutbound calls = %d, %d", first, second)
	}
}

func TestCallbackThresholdBatchesWithoutTasks(t *testing.T) {
	outbound := make(chan int, 2)
	events := &Events{Pollers: 1, WriteBufferedThreshold: 16}
	events.OnOpen = func(conn Conn) {
		if err := conn.WriteByte('a'); err != nil {
			t.Error(err)
		}
		if err := conn.WriteByte('b'); err != nil {
			t.Error(err)
		}
		if err := conn.WriteByte('c'); err != nil {
			t.Error(err)
		}
	}
	events.OnOutbound = func(_ Conn, written int) { outbound <- written }
	testConn := newTestConnection(t, events)
	if got := string(readPeer(t, testConn.peer, 3)); got != "abc" {
		t.Fatalf("peer received %q", got)
	}
	if written := <-outbound; written != 3 {
		t.Fatalf("OnOutbound bytes = %d", written)
	}
	select {
	case extra := <-outbound:
		t.Fatalf("threshold write used an extra flush of %d bytes", extra)
	default:
	}
	if testConn.conn.queuedWrites.Load() != 0 || testConn.conn.loop.tasks.HasPending() {
		t.Fatal("callback threshold write created a queued task")
	}
}

func TestConcurrentWriteAndCloseDoesNotLoseAcceptedData(t *testing.T) {
	opened := make(chan *fdConn, 1)
	releaseOpen := make(chan struct{})
	closed := make(chan error, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: 1 << 20, MaxPendingWrites: 128}
	events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn); <-releaseOpen }
	events.OnClose = func(_ Conn, err error) { closed <- err }
	testConn, registered := startTestConnection(t, events)
	conn := <-opened

	accepted := make(map[string]bool)
	for index := 0; index < 16; index++ {
		message := fmt.Sprintf("%08d", index)
		if _, err := conn.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
		accepted[message] = true
	}

	type writeResult struct {
		message string
		err     error
	}
	results := make(chan writeResult, 32)
	start := make(chan struct{})
	var writers sync.WaitGroup
	for index := 16; index < 48; index++ {
		writers.Add(1)
		go func(index int) {
			defer writers.Done()
			<-start
			message := fmt.Sprintf("%08d", index)
			_, err := conn.Write([]byte(message))
			results <- writeResult{message: message, err: err}
		}(index)
	}
	closeResult := make(chan error, 1)
	go func() { <-start; closeResult <- conn.CloseWith(errors.New("concurrent close")) }()
	close(start)
	writers.Wait()
	close(results)
	for result := range results {
		if result.err == nil {
			accepted[result.message] = true
		} else if !errors.Is(result.err, net.ErrClosed) {
			t.Fatalf("Write error = %v", result.err)
		}
	}
	if err := <-closeResult; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("CloseWith error = %v", err)
	}
	close(releaseOpen)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if errors.Is(err, ErrUnflushedData) {
			t.Fatalf("small accepted writes were not flushed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("connection did not close")
	}

	payload := readPeer(t, testConn.peer, len(accepted)*8)
	seen := make(map[string]bool, len(accepted))
	for offset := 0; offset < len(payload); offset += 8 {
		seen[string(payload[offset:offset+8])] = true
	}
	if len(seen) != len(accepted) {
		t.Fatalf("received %d unique messages, accepted %d", len(seen), len(accepted))
	}
	for message := range accepted {
		if !seen[message] {
			t.Fatalf("accepted message %q was not sent", message)
		}
	}
	if pending, queued := conn.pending.Load(), conn.queuedWrites.Load(); pending != 0 || queued != 0 {
		t.Fatalf("counters after Close = pending %d, queued %d", pending, queued)
	}
}

func TestDeadlineClearAndExpire(t *testing.T) {
	closed := make(chan error, 1)
	events := &Events{Pollers: 1, OnClose: func(_ Conn, err error) { closed <- err }}
	testConn := newTestConnection(t, events)
	if err := testConn.conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := testConn.conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		t.Fatalf("cleared deadline closed connection: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := testConn.conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("deadline error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline did not close connection")
	}
}

func TestBackpressureHysteresis(t *testing.T) {
	events := &Events{MaxOutboundBuffered: 100}
	conn := &fdConn{}
	conn.events = events
	conn.pending.Store(75)
	conn.outbound.AppendOwned(bytebuf.CloneBuffer([]byte("x")))
	if got := conn.desiredInterest(); got&poller.Readable != 0 || got&poller.Writable == 0 {
		t.Fatalf("high-water interest = %v", got)
	}
	conn.pending.Store(60)
	if got := conn.desiredInterest(); got&poller.Readable != 0 {
		t.Fatalf("hysteresis interest = %v", got)
	}
	conn.pending.Store(50)
	conn.outbound.Reset()
	if got := conn.desiredInterest(); got != poller.Readable {
		t.Fatalf("low-water interest = %v", got)
	}
}

func TestUDPChildReportsUnflushedBytes(t *testing.T) {
	cause := errors.New("closed")
	closed := make(chan error, 1)
	events := &Events{OnClose: func(_ Conn, err error) { closed <- err }}
	server := &fdConn{udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)}}
	child := &fdConn{udp: &unixUDPState{server: server}}
	child.events = events
	child.remoteAddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	child.udp.key = socket.UDPAddress{Port: 1}
	server.udp.peers[child.udp.key] = child
	child.pending.Store(7)
	child.closeOnLoop(cause)
	err := <-closed
	if !errors.Is(err, cause) || !errors.Is(err, ErrUnflushedData) {
		t.Fatalf("close error = %v", err)
	}
	var unflushed UnflushedError
	if !errors.As(err, &unflushed) || unflushed.Remaining != 7 {
		t.Fatalf("unflushed error = %#v", unflushed)
	}
	if len(server.udp.peers) != 0 {
		t.Fatalf("closed UDP child left %d peer entries", len(server.udp.peers))
	}
}

func TestServeDialAndShutdownLifecycle(t *testing.T) {
	started := make(chan string, 1)
	dialOpened := make(chan struct{})
	received := make(chan string, 1)
	stopped := make(chan struct{})
	var dialOpenOnce sync.Once

	events := &Events{Pollers: 2}
	events.OnStart = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.laddr.String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	events.OnOpen = func(conn Conn) {
		fdConn := conn.(*fdConn)
		if fdConn.loop == events.master {
			t.Errorf("TCP connection registered on the listener loop")
		}
		if conn.Userdata() == "dial" {
			dialOpenOnce.Do(func() { close(dialOpened) })
		}
	}
	events.OnData = func(conn Conn) error {
		if conn.Userdata() != "dial" {
			_, err := conn.WriteTo(conn)
			return err
		}
		data := make([]byte, conn.InboundBuffered())
		n, err := conn.Read(data)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		received <- string(data[:n])
		return nil
	}
	events.OnStop = func(*Events) { close(stopped) }

	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	var address string
	select {
	case address = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not start")
	}

	dialed, err := events.DialContext(context.Background(), "tcp://"+address, "dial")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-dialOpened:
	default:
		t.Fatal("Dial returned before OnOpen")
	}
	if n, err := dialed.Write([]byte("ping")); err != nil || n != 4 {
		t.Fatalf("Dial connection Write = %d, %v", n, err)
	}
	select {
	case got := <-received:
		if got != "ping" {
			t.Fatalf("received %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("echo did not arrive")
	}

	shutdownErr := errors.New("test shutdown")
	closeDone := make(chan error, 1)
	go func() { closeDone <- events.Close(shutdownErr) }()
	select {
	case err = <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events.Close did not return")
	}
	select {
	case err = <-serveDone:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("OnStop was not called")
	}
}

func TestInboundLimitClosesConnection(t *testing.T) {
	closed := make(chan error, 1)
	events := &Events{
		Pollers:            1,
		MaxInboundBuffered: 4,
		OnData:             func(Conn) error { return nil },
		OnClose:            func(_ Conn, err error) { closed <- err },
	}
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("12345")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, ErrInboundOverflow) {
			t.Fatalf("close error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("inbound overflow did not close connection")
	}
}

func TestEventsCloseFromCallbackDoesNotDeadlock(t *testing.T) {
	started := make(chan string, 1)
	closeReturned := make(chan struct{})
	shutdownErr := errors.New("callback shutdown")
	events := &Events{Pollers: 1}
	events.OnStart = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.laddr.String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	events.OnData = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		if err := events.Close(shutdownErr); err != nil {
			return err
		}
		close(closeReturned)
		return nil
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	address := <-started
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Write([]byte("stop")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closeReturned:
	case <-time.After(time.Second):
		t.Fatal("Events.Close blocked inside OnData")
	}
	select {
	case err = <-serveDone:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestListenerInitializationFailureRollsBackLoops(t *testing.T) {
	events := &Events{Pollers: 1}
	done := make(chan error, 1)
	go func() { done <- events.Serve("unsupported://address") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve succeeded with unsupported protocol")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initialization rollback did not finish")
	}
}

func TestUDPChildCloseDoesNotCloseSharedServer(t *testing.T) {
	started := make(chan string, 1)
	childClosed := make(chan struct{}, 2)
	events := &Events{Pollers: 1}
	events.OnStart = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.laddr.String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	events.OnData = func(conn Conn) error {
		data := make([]byte, conn.InboundBuffered())
		n, _ := conn.Read(data)
		if string(data[:n]) == "close" {
			return conn.Close()
		}
		_, err := conn.Write(data[:n])
		return err
	}
	events.OnClose = func(Conn, error) { childClosed <- struct{}{} }
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("udp://127.0.0.1:0") }()
	address := <-started
	serverAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	first, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err = first.Write([]byte("close")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-childClosed:
	case <-time.After(time.Second):
		t.Fatal("first UDP child did not close")
	}
	if _, err = second.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err = second.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 16)
	n, err := second.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buffer[:n]); got != "ping" {
		t.Fatalf("UDP echo = %q", got)
	}

	shutdownErr := errors.New("udp shutdown")
	if err = events.Close(shutdownErr); err != nil {
		t.Fatal(err)
	}
	if err = <-serveDone; !errors.Is(err, shutdownErr) {
		t.Fatalf("Serve error = %v", err)
	}
	select {
	case <-childClosed:
	case <-time.After(time.Second):
		t.Fatal("remaining UDP child did not close on shutdown")
	}
}
