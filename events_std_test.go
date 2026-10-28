//go:build windows || stdio

package uio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type stdRegistrationConn struct {
	fd             uintptr
	closed         chan struct{}
	readData       chan []byte
	closeOnce      sync.Once
	openReturned   atomic.Bool
	orderViolation atomic.Bool
	readActive     atomic.Int32
	maxReadActive  atomic.Int32
	readCalls      atomic.Int32
	secondRead     chan struct{}
	secondReadOnce sync.Once
	writeCalls     atomic.Int32
	writeStarted   chan struct{}
	writeRelease   chan struct{}
	writeOnce      sync.Once
	writeFunc      func([]byte) (int, error)
	readFunc       func([]byte) (int, error)
}

func newStdRegistrationConn(fd uintptr) *stdRegistrationConn {
	return &stdRegistrationConn{
		fd: fd, closed: make(chan struct{}), readData: make(chan []byte),
	}
}

func (conn *stdRegistrationConn) Read(buffer []byte) (int, error) {
	if conn.readCalls.Add(1) == 2 && conn.secondRead != nil {
		conn.secondReadOnce.Do(func() { close(conn.secondRead) })
	}
	active := conn.readActive.Add(1)
	defer conn.readActive.Add(-1)
	for maximum := conn.maxReadActive.Load(); active > maximum; maximum = conn.maxReadActive.Load() {
		if conn.maxReadActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	if !conn.openReturned.Load() {
		conn.orderViolation.Store(true)
	}
	if conn.readFunc != nil {
		return conn.readFunc(buffer)
	}
	select {
	case data := <-conn.readData:
		return copy(buffer, data), nil
	case <-conn.closed:
		return 0, net.ErrClosed
	}
}

func (conn *stdRegistrationConn) Write(buffer []byte) (int, error) {
	conn.writeCalls.Add(1)
	if !conn.openReturned.Load() {
		conn.orderViolation.Store(true)
	}
	if conn.writeStarted != nil {
		conn.writeOnce.Do(func() { close(conn.writeStarted) })
		<-conn.closed
		if conn.writeRelease != nil {
			<-conn.writeRelease
		}
		return 0, net.ErrClosed
	}
	if conn.writeFunc != nil {
		return conn.writeFunc(buffer)
	}
	return len(buffer), nil
}

func (conn *stdRegistrationConn) Close() error {
	conn.closeOnce.Do(func() { close(conn.closed) })
	return nil
}

func (*stdRegistrationConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*stdRegistrationConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*stdRegistrationConn) SetDeadline(time.Time) error      { return nil }
func (*stdRegistrationConn) SetReadDeadline(time.Time) error  { return nil }
func (*stdRegistrationConn) SetWriteDeadline(time.Time) error { return nil }
func (conn *stdRegistrationConn) SyscallConn() (syscall.RawConn, error) {
	return stdRawConn(conn.fd), nil
}

type stdRawConn uintptr

func (fd stdRawConn) Control(callback func(uintptr)) error {
	callback(uintptr(fd))
	return nil
}

func (fd stdRawConn) Read(callback func(uintptr) bool) error {
	callback(uintptr(fd))
	return nil
}

func (fd stdRawConn) Write(callback func(uintptr) bool) error {
	callback(uintptr(fd))
	return nil
}

func startStdTestEvents(t *testing.T, events *Events) <-chan error {
	t.Helper()
	started := make(chan struct{})
	funcs(events).Start = func(*Events) { close(started) }
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stdio events did not start")
	}
	return serveDone
}

func newStdFDConn(events *Events, raw *stdRegistrationConn) *fdConn {
	conn := &fdConn{
		conn: raw, writeSig: make(chan struct{}, 1),
	}
	conn.entry = events.plainEntry()
	conn.loop = events.workers[0]
	return conn
}

func stopStdTestEvents(t *testing.T, events *Events, serveDone <-chan error, closeErr error) {
	t.Helper()
	closeDone := make(chan error, 1)
	go func() { closeDone <- events.Close(closeErr) }()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for closeDone != nil || serveDone != nil {
		select {
		case err := <-closeDone:
			if err != nil {
				t.Fatalf("Events.Close error = %v", err)
			}
			closeDone = nil
		case err := <-serveDone:
			if !errors.Is(err, closeErr) {
				t.Fatalf("Events.Serve error = %v", err)
			}
			serveDone = nil
		case <-timer.C:
			t.Fatal("stdio events did not stop within one second")
		}
	}
}

func TestStdRegisterStartsIOAfterOnOpen(t *testing.T) {
	events := &Events{Pollers: 1, MaxBufferSize: 64}
	openEntered := make(chan struct{})
	releaseOpen := make(chan struct{})
	dataCalled := make(chan struct{}, 1)
	callbackErr := make(chan error, 1)
	funcs(events).Open = func(conn Conn) {
		raw := conn.(*fdConn).conn.(*stdRegistrationConn)
		close(openEntered)
		<-releaseOpen
		if _, err := conn.Write([]byte("x")); err != nil {
			callbackErr <- err
		}
		raw.openReturned.Store(true)
	}
	funcs(events).Read = func(conn Conn) error {
		raw := conn.(*fdConn).conn.(*stdRegistrationConn)
		if !raw.openReturned.Load() {
			raw.orderViolation.Store(true)
		}
		_, err := conn.Discard(-1)
		dataCalled <- struct{}{}
		return err
	}
	serveDone := startStdTestEvents(t, events)

	raw := newStdRegistrationConn(0)
	conn := newStdFDConn(events, raw)
	registerDone := make(chan error, 1)
	task := acquireTask(registerTask, conn)
	task.done = registerDone
	if !conn.loop.submitTask(task) {
		releaseTask(task)
		t.Fatal("register task was rejected")
	}

	select {
	case <-openEntered:
	case <-time.After(time.Second):
		t.Fatal("OnOpen was not called")
	}
	time.Sleep(20 * time.Millisecond)
	if raw.readActive.Load() != 0 || raw.writeCalls.Load() != 0 {
		t.Fatal("I/O goroutine started before OnOpen returned")
	}
	close(releaseOpen)
	select {
	case err := <-registerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("register task did not return")
	}
	select {
	case err := <-callbackErr:
		t.Fatal(err)
	default:
	}

	deadline := time.Now().Add(time.Second)
	for raw.readActive.Load() != 1 || raw.writeCalls.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("I/O did not start: active reads %d, writes %d", raw.readActive.Load(), raw.writeCalls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case raw.readData <- []byte("data"):
	case <-time.After(time.Second):
		t.Fatal("read goroutine did not accept data")
	}
	select {
	case <-dataCalled:
	case <-time.After(time.Second):
		t.Fatal("OnRead was not called")
	}
	if raw.orderViolation.Load() {
		t.Fatal("I/O callback ran before OnOpen returned")
	}
	if raw.maxReadActive.Load() != 1 || raw.writeCalls.Load() != 1 {
		t.Fatalf("connection started duplicate I/O: concurrent reads %d, writes %d", raw.maxReadActive.Load(), raw.writeCalls.Load())
	}

	stopStdTestEvents(t, events, serveDone, errors.New("ordered shutdown"))
}

// A burst of registrations cut short by shutdown: every registration
// returns, no connection starts I/O before its OnOpen returns, and OnOpen and
// OnClose stay paired, for connections that shutdown closed before their read
// goroutine ran too.
func TestStdRegisterBurstAndClose(t *testing.T) {
	const desiredTotal = 2048
	total := min(desiredTotal, stdTestFDLimit())
	events := &Events{Pollers: 1, MaxBufferSize: 64}
	var opened, closed atomic.Int32
	funcs(events).Open = func(conn Conn) {
		conn.(*fdConn).conn.(*stdRegistrationConn).openReturned.Store(true)
		opened.Add(1)
	}
	funcs(events).Close = func(Conn, error) { closed.Add(1) }
	serveDone := startStdTestEvents(t, events)

	results := make([]chan error, total)
	rawConns := make([]*stdRegistrationConn, total)
	for i := 0; i < total; i++ {
		raw := newStdRegistrationConn(uintptr(i))
		rawConns[i] = raw
		conn := newStdFDConn(events, raw)
		task := acquireTask(registerTask, conn)
		results[i] = make(chan error, 1)
		task.done = results[i]
		if !conn.loop.submitTask(task) {
			releaseTask(task)
			t.Fatalf("register task %d was rejected", i)
		}
	}

	stopStdTestEvents(t, events, serveDone, errors.New("burst shutdown"))
	if o, c := opened.Load(), closed.Load(); o != c || int(o) > total {
		t.Fatalf("OnOpen called %d times and OnClose %d for %d connections, want them paired", o, c, total)
	}
	for i, result := range results {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("register task %d returned %v", i, err)
			}
		default:
			t.Fatalf("register task %d did not return", i)
		}
		if rawConns[i].orderViolation.Load() {
			t.Fatalf("connection %d started I/O before OnOpen returned", i)
		}
	}
}

func TestStdFlushDoesNotWaitForBlockedWriter(t *testing.T) {
	events := &Events{Pollers: 1, MaxBufferSize: 64}
	funcs(events).Open = func(conn Conn) {
		conn.(*fdConn).conn.(*stdRegistrationConn).openReturned.Store(true)
	}
	serveDone := startStdTestEvents(t, events)

	raw := newStdRegistrationConn(0)
	raw.writeStarted = make(chan struct{})
	raw.writeRelease = make(chan struct{})
	conn := newStdFDConn(events, raw)
	registerDone := make(chan error, 1)
	task := acquireTask(registerTask, conn)
	task.done = registerDone
	if !conn.loop.submitTask(task) {
		releaseTask(task)
		t.Fatal("register task was rejected")
	}
	select {
	case err := <-registerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("register task did not return")
	}

	if n, err := conn.Write([]byte("blocked")); err != nil || n != len("blocked") {
		t.Fatalf("Write = %d, %v", n, err)
	}
	select {
	case <-raw.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write loop did not enter the blocking write")
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- conn.Flush() }()
	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Flush waited for blocking socket I/O")
	}

	type writeResult struct {
		n   int
		err error
	}
	writeDone := make(chan writeResult, 1)
	go func() {
		n, err := conn.Write([]byte("concurrent"))
		writeDone <- writeResult{n: n, err: err}
	}()
	select {
	case result := <-writeDone:
		if result.err != nil || result.n != len("concurrent") {
			t.Fatalf("concurrent Write = %d, %v", result.n, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent Write waited for the blocking socket write")
	}
	bufferedDone := make(chan int, 1)
	go func() { bufferedDone <- conn.OutboundBuffered() }()
	select {
	case buffered := <-bufferedDone:
		if want := len("blocked") + len("concurrent"); buffered != want {
			t.Fatalf("OutboundBuffered = %d, want %d", buffered, want)
		}
	case <-time.After(time.Second):
		t.Fatal("OutboundBuffered waited for the blocking socket write")
	}

	shutdownErr := errors.New("blocked flush shutdown")
	if err := events.Close(shutdownErr); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- events.Wait() }()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-raw.closed:
	case <-timer.C:
		t.Fatal("Close did not interrupt the blocking socket write")
	}
	select {
	case <-waitDone:
		t.Fatal("Wait returned before the blocked writer released its payload")
	default:
	}
	close(raw.writeRelease)

	for waitDone != nil || serveDone != nil {
		select {
		case err := <-waitDone:
			if !errors.Is(err, shutdownErr) {
				t.Fatalf("Events.Wait error = %v", err)
			}
			waitDone = nil
		case err := <-serveDone:
			if !errors.Is(err, shutdownErr) {
				t.Fatalf("Events.Serve error = %v", err)
			}
			serveDone = nil
		case <-timer.C:
			t.Fatal("Close or Serve did not return within one second")
		}
	}
	if buffered := conn.OutboundBuffered(); buffered != 0 {
		t.Fatalf("outbound bytes after Close = %d", buffered)
	}
}

func TestStdWritevCopiesBeforeReturnAndDoesNotRetainVector(t *testing.T) {
	conn := &fdConn{
		commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: -1}).plainEntry()},
		writeSig:   make(chan struct{}, 1),
	}
	first := []byte("header")
	second := []byte("payload")
	vec := [][]byte{first, second}
	if n, err := conn.Writev(vec); err != nil || n != len("headerpayload") {
		t.Fatalf("Writev() = %d, %v", n, err)
	}
	copy(first, "xxxxxx")
	copy(second, "yyyyyyy")
	clear(vec)

	conn.mux.Lock()
	got := make([]byte, conn.outbound.Len())
	_, _ = conn.outbound.Read(got)
	conn.mux.Unlock()
	if string(got) != "headerpayload" {
		t.Fatalf("outbound = %q, want headerpayload", got)
	}
}

func TestStdLargeWritevUsesOwnedBuffersAndSmallWritesCoalesce(t *testing.T) {
	t.Run("large Writev", func(t *testing.T) {
		conn := &fdConn{
			commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: -1}).plainEntry()},
			writeSig:   make(chan struct{}, 1),
		}
		header := []byte("header")
		payload := bytes.Repeat([]byte{'p'}, stdOwnedWriteThreshold)
		want := append(append([]byte(nil), header...), payload...)
		if n, err := conn.Writev([][]byte{header, payload}); err != nil || n != len(want) {
			t.Fatalf("Writev = %d, %v", n, err)
		}
		clear(header)
		clear(payload)

		conn.mux.Lock()
		var storage [8][]byte
		buffers, size := conn.outbound.PeekVecN(storage[:0], len(storage))
		var got []byte
		for _, buffer := range buffers {
			got = append(got, buffer...)
		}
		conn.outbound.Reset()
		conn.mux.Unlock()
		if len(buffers) != 2 || size != len(want) {
			t.Fatalf("outbound vectors/bytes = %d/%d, want 2/%d", len(buffers), size, len(want))
		}
		if !bytes.Equal(got, want) {
			t.Fatal("owned Writev did not retain its payload copy")
		}
	})

	t.Run("small writes", func(t *testing.T) {
		conn := &fdConn{
			commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: -1}).plainEntry()},
			writeSig:   make(chan struct{}, 1),
		}
		if _, err := conn.Write([]byte{'a'}); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte{'b'}); err != nil {
			t.Fatal(err)
		}
		conn.mux.Lock()
		var storage [8][]byte
		buffers, size := conn.outbound.PeekVecN(storage[:0], len(storage))
		var got []byte
		if len(buffers) > 0 {
			got = append(got, buffers[0]...)
		}
		conn.outbound.Reset()
		conn.mux.Unlock()
		if len(buffers) != 1 || size != 2 || string(got) != "ab" {
			t.Fatalf("small-write vectors/bytes/payload = %d/%d/%q", len(buffers), size, got)
		}
	})

	t.Run("many tiny vectors", func(t *testing.T) {
		conn := &fdConn{
			commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: -1}).plainEntry()},
			writeSig:   make(chan struct{}, 1),
		}
		vec := make([][]byte, stdOwnedWriteThreshold)
		for index := range vec {
			vec[index] = []byte{'x'}
		}
		if n, err := conn.Writev(vec); err != nil || n != len(vec) {
			t.Fatalf("Writev = %d, %v", n, err)
		}
		conn.mux.Lock()
		var storage [8][]byte
		buffers, size := conn.outbound.PeekVecN(storage[:0], len(storage))
		capacity := conn.outbound.Cap()
		conn.outbound.Reset()
		conn.mux.Unlock()
		if len(buffers) != 1 || size != len(vec) || capacity > 2*len(vec) {
			t.Fatalf("tiny-vector buffers/bytes/capacity = %d/%d/%d", len(buffers), size, capacity)
		}
	})
}

func BenchmarkStdWritev1MiB(b *testing.B) {
	conn := &fdConn{
		commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: -1}).plainEntry()},
		writeSig:   make(chan struct{}, 1),
	}
	header := make([]byte, 10)
	payload := make([]byte, 1<<20)
	vec := [][]byte{header, payload}
	wireSize := len(header) + len(payload)
	b.ReportAllocs()
	b.SetBytes(int64(wireSize))
	b.ResetTimer()
	for range b.N {
		if n, err := conn.Writev(vec); err != nil || n != wireSize {
			b.Fatalf("Writev() = %d, %v", n, err)
		}
		conn.mux.Lock()
		conn.outbound.Reset()
		conn.mux.Unlock()
	}
}

func BenchmarkStdConcurrentWritev64KiB(b *testing.B) {
	header := make([]byte, 10)
	payload := make([]byte, 64<<10)
	vec := [][]byte{header, payload}
	wireSize := len(header) + len(payload)

	for _, benchmark := range []struct {
		name  string
		write func(*fdConn)
	}{
		{
			name: "owned-outside-lock",
			write: func(conn *fdConn) {
				if n, err := conn.Writev(vec); err != nil || n != wireSize {
					panic("Writev failed")
				}
				conn.mux.Lock()
				conn.outbound.Reset()
				conn.mux.Unlock()
			},
		},
		{
			name: "copy-under-lock",
			write: func(conn *fdConn) {
				conn.mux.Lock()
				_, _ = conn.outbound.Writev(vec)
				conn.outbound.Reset()
				conn.mux.Unlock()
			},
		},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			conn := &fdConn{
				commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: -1}).plainEntry()},
				writeSig:   make(chan struct{}, 1),
			}
			b.ReportAllocs()
			b.SetBytes(int64(wireSize))
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					benchmark.write(conn)
				}
			})
		})
	}
}

func BenchmarkStdWriteOwned1MiB(b *testing.B) {
	conn := &fdConn{
		commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: -1}).plainEntry()},
		writeSig:   make(chan struct{}, 1),
	}
	payload := make([]byte, 1<<20)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		buffer := AcquireBuffer(len(payload))
		_, _ = buffer.Write(payload)
		if n, err := conn.WriteOwned(buffer); err != nil || n != len(payload) {
			b.Fatalf("WriteOwned() = %d, %v", n, err)
		}
		conn.mux.Lock()
		conn.outbound.Reset()
		conn.mux.Unlock()
		select {
		case <-conn.writeSig:
		default:
		}
	}
}

func TestStdWriteOwnedTransfersBufferWithoutCopy(t *testing.T) {
	conn := &fdConn{
		commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: 16}).plainEntry()},
		writeSig:   make(chan struct{}, 1),
	}
	buffer := AcquireBuffer(8)
	dst := buffer.AvailableBuffer()[:8]
	encoded := &dst[0]
	buffer.CommitWrite(copy(dst, "payload"))
	n, err := conn.WriteOwned(buffer)
	if err != nil || n != 7 {
		t.Fatalf("WriteOwned = %d, %v", n, err)
	}
	peeked := conn.outbound.Peek(make([]byte, 7))
	if string(peeked) != "payload" || &peeked[0] != encoded {
		t.Fatal("owned write was copied before entering outbound")
	}
	conn.outbound.Reset()
}

func TestStdUDPWriteOwnedSendsOneDatagram(t *testing.T) {
	receiver, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := net.DialUDP("udp", nil, receiver.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	conn := &fdConn{udp: &stdUDPState{sock: sender}}
	conn.entry = (&Events{}).plainEntry()

	buffer := AcquireBuffer(8)
	_, _ = buffer.WriteString("datagram")
	if n, err := conn.WriteOwned(buffer); err != nil || n != 8 {
		t.Fatalf("WriteOwned = %d, %v", n, err)
	}
	if err = receiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, 16)
	n, _, err := receiver.ReadFromUDP(received)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(received[:n]); got != "datagram" {
		t.Fatalf("datagram = %q, want datagram", got)
	}
}

func TestStdDrainOutboundPreservesWritesQueuedDuringIO(t *testing.T) {
	raw := newStdRegistrationConn(30002)
	raw.openReturned.Store(true)
	writeStarted := make(chan struct{})
	writeRelease := make(chan struct{})
	var firstWrite atomic.Bool
	var receivedMu sync.Mutex
	var received []byte
	raw.writeFunc = func(buffer []byte) (int, error) {
		if firstWrite.CompareAndSwap(false, true) {
			close(writeStarted)
			<-writeRelease
		}
		receivedMu.Lock()
		received = append(received, buffer...)
		receivedMu.Unlock()
		return len(buffer), nil
	}
	conn := &fdConn{
		conn: raw, writeSig: make(chan struct{}, 1),
	}
	conn.entry = (&Events{MaxOutboundBuffered: 3}).plainEntry()
	conn.events().callbackWG.Add(1)
	writeLoopDone := make(chan struct{})
	go func() {
		conn.writeLoop()
		close(writeLoopDone)
	}()
	defer func() {
		// Closing wakes the write loop through its own notification channel.
		conn.closing.Store(true)
		select {
		case conn.writeSig <- struct{}{}:
		default:
		}
		select {
		case <-writeLoopDone:
		case <-time.After(time.Second):
			t.Error("write loop did not stop")
		}
	}()

	if n, err := conn.Write([]byte("A")); err != nil || n != 1 {
		t.Fatalf("first Write = %d, %v", n, err)
	}
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("first batch did not enter socket Write")
	}
	writeDone := make(chan error, 1)
	go func() {
		n, err := conn.Write([]byte("B"))
		if err == nil && n != 1 {
			err = io.ErrShortWrite
		}
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second Write waited for the blocking socket write")
	}
	if buffered := conn.OutboundBuffered(); buffered != 2 {
		t.Fatalf("OutboundBuffered during write = %d, want 2", buffered)
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- conn.Flush() }()
	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Flush waited for the blocked writer")
	}
	if n, err := conn.Write([]byte("C")); err != nil || n != 1 {
		t.Fatalf("post-Flush Write = %d, %v", n, err)
	}
	if n, err := conn.Write([]byte("D")); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("overflow Write = %d, %v", n, err)
	}
	close(writeRelease)
	deadline := time.Now().Add(time.Second)
	for {
		receivedMu.Lock()
		got := string(received)
		receivedMu.Unlock()
		buffered := conn.OutboundBuffered()
		if got == "ABC" && buffered == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket received %q with %d buffered bytes, want ABC/0", got, buffered)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStdDrainOutboundClearsVectorStorage pins that a finished drain returns
// its write vector to the pool cleared: a stale buffer left in a slot would
// pin the block it references for as long as the pooled vector lives.
func TestStdDrainOutboundClearsVectorStorage(t *testing.T) {
	// Pin the pool's per-P shard so the poisoned vector is the one the drain
	// below fetches and returns.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	poisoned := stdWriteScratchPool.Get().(*stdWriteScratch)
	for index := range poisoned.vec {
		poisoned.vec[index] = []byte("stale")
	}
	poisoned.bufs = net.Buffers{[]byte("stale")}
	stdWriteScratchPool.Put(poisoned)

	raw := newStdRegistrationConn(30003)
	raw.openReturned.Store(true)
	conn := &fdConn{commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: -1}).plainEntry()}, conn: raw}
	if _, err := conn.outbound.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if written, err := conn.drainOutbound(); err != nil || written != len("payload") {
		t.Fatalf("drainOutbound = %d, %v", written, err)
	}
	returned := stdWriteScratchPool.Get().(*stdWriteScratch)
	defer stdWriteScratchPool.Put(returned)
	for index, buffer := range returned.vec {
		if buffer != nil {
			t.Fatalf("vector storage slot %d retained %d bytes", index, len(buffer))
		}
	}
	if returned.bufs != nil {
		t.Fatal("scratch retained its Buffers wrapper")
	}
}

func TestStdFlushCoalescesWakeWithoutAllocating(t *testing.T) {
	conn := &fdConn{writeSig: make(chan struct{}, 1)}
	conn.entry = (&Events{}).plainEntry()

	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(conn.writeSig) != 1 {
		t.Fatal("Flush did not signal the write loop")
	}

	var flushErr error
	allocations := testing.AllocsPerRun(1000, func() {
		flushErr = conn.Flush()
	})
	if flushErr != nil {
		t.Fatal(flushErr)
	}
	if allocations != 0 {
		t.Fatalf("Flush allocations = %.2f, want 0", allocations)
	}
	if len(conn.writeSig) != 1 {
		t.Fatal("Flush did not coalesce write-loop wakeups")
	}
}

func TestStdServeEchoAndShutdown(t *testing.T) {
	started := make(chan string, 1)
	opened := make(chan Conn, 1)
	dataCalls := make(chan struct{}, 2)
	events := &Events{Pollers: 1, WriteBufferedThreshold: 1}
	funcs(events).Start = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.ln.Addr().String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	funcs(events).Open = func(conn Conn) { opened <- conn }
	funcs(events).Read = func(conn Conn) error {
		dataCalls <- struct{}{}
		_, err := conn.WriteTo(conn)
		return err
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()

	var address string
	select {
	case address = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("stdio server did not start")
	}
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4)
	if _, err = io.ReadFull(client, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "ping" {
		t.Fatalf("echo = %q", buffer)
	}
	<-dataCalls
	accepted := <-opened
	if err = accepted.Wake(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dataCalls:
	case <-time.After(time.Second):
		t.Fatal("stdio Wake did not invoke OnRead")
	}

	shutdownErr := errors.New("stdio shutdown")
	if err = events.Close(shutdownErr); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-serveDone:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stdio Serve did not stop")
	}
}

func TestStdEventsCloseFromOnInboundDoesNotDeadlock(t *testing.T) {
	started := make(chan string, 1)
	closeReturned := make(chan struct{})
	shutdownErr := errors.New("stdio callback shutdown")
	events := &Events{Pollers: 1}
	funcs(events).Start = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.ln.Addr().String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	funcs(events).Read = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		if err := events.Close(shutdownErr); err != nil {
			return err
		}
		close(closeReturned)
		return nil
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	var addr string
	select {
	case addr = <-started:
	case err := <-serveDone:
		t.Fatalf("Serve did not start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not start")
	}
	client, err := net.Dial("tcp", addr)
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
		t.Fatal("Events.Close blocked in stdio OnRead")
	}
	select {
	case err = <-serveDone:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stdio Serve did not stop")
	}
}

func TestStdWaitWaitsForLifetimeCallback(t *testing.T) {
	started := make(chan string, 1)
	callbackEntered := make(chan struct{}, 1)
	releaseCallback := make(chan struct{})
	shutdownErr := errors.New("external close during std callback")
	var callbackOnce sync.Once

	events := &Events{Pollers: 1}
	funcs(events).Start = func(events *Events) {
		events.acceptor.mux.Lock()
		defer events.acceptor.mux.Unlock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.ln.Addr().String()
			return
		}
	}
	funcs(events).Read = func(conn Conn) error {
		callbackOnce.Do(func() {
			callbackEntered <- struct{}{}
			<-releaseCallback
		})
		_, _ = conn.Discard(-1)
		return nil
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	var addr string
	select {
	case addr = <-started:
	case err := <-serveDone:
		t.Fatalf("Serve did not start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not start")
	}
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Write([]byte("block")); err != nil {
		t.Fatal(err)
	}
	<-callbackEntered

	if err = events.Close(shutdownErr); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- events.Wait() }()
	select {
	case <-waitDone:
		t.Fatal("Wait returned while OnRead was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseCallback)

	select {
	case err = <-waitDone:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("Wait error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not finish after OnRead returned")
	}
	select {
	case err = <-serveDone:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestStdReadLoopKeepsLifetimeUntilExit(t *testing.T) {
	raw := newStdRegistrationConn(30004)
	raw.openReturned.Store(true)
	raw.secondRead = make(chan struct{})
	events := &Events{MaxBufferSize: 64}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	callbackDone := make(chan struct{}, 1)
	funcs(events).Read = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		callbackDone <- struct{}{}
		return nil
	}
	conn := &fdConn{conn: raw}
	conn.entry = events.plainEntry()
	events.callbackWG.Add(1)
	readDone := make(chan struct{})
	go func() {
		conn.readLoop()
		close(readDone)
	}()

	raw.readData <- []byte("first")
	<-callbackDone
	select {
	case <-raw.secondRead:
	case <-time.After(time.Second):
		t.Fatal("read loop did not start its second read")
	}
	waitDone := make(chan struct{})
	go func() {
		events.callbackWG.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("callback lifetime ended between reads")
	case <-time.After(50 * time.Millisecond):
	}

	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("read loop did not exit")
	}
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("callback lifetime remained after read loop exit")
	}
}

func TestStdReadLoopDeliversPayloadBeforeTerminalError(t *testing.T) {
	raw := newStdRegistrationConn(30005)
	raw.openReturned.Store(true)
	var once sync.Once
	raw.readFunc = func(buffer []byte) (int, error) {
		n := 0
		once.Do(func() { n = copy(buffer, "tail") })
		return n, io.EOF
	}
	events := &Events{MaxBufferSize: 64}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	funcs(events).Read = func(conn Conn) error {
		chunk := append([]byte(nil), conn.PeekChunk()...)
		_, _ = conn.Discard(-1)
		received <- string(chunk)
		return nil
	}
	conn := &fdConn{conn: raw}
	conn.entry = events.plainEntry()
	events.callbackWG.Add(1)
	done := make(chan struct{})
	go func() {
		conn.readLoop()
		close(done)
	}()
	select {
	case got := <-received:
		if got != "tail" {
			t.Fatalf("final payload = %q, want tail", got)
		}
	case <-time.After(time.Second):
		t.Fatal("final payload was not delivered")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("read loop did not exit after terminal error")
	}
}

// A close requested while a read is returning its bytes may deliver OnClose
// before the read loop takes the callback mutex; the loop must then drop the
// bytes rather than run OnRead after the final callback.
func TestStdReadLoopRunsNoDataAfterOnClose(t *testing.T) {
	events := &Events{Pollers: 1, MaxBufferSize: 64}
	closed := make(chan struct{})
	var closedFlag, dataAfterClose atomic.Bool
	funcs(events).Close = func(Conn, error) {
		closedFlag.Store(true)
		close(closed)
	}
	funcs(events).Read = func(conn Conn) error {
		if closedFlag.Load() {
			dataAfterClose.Store(true)
		}
		_, err := conn.Discard(-1)
		return err
	}
	serveDone := startStdTestEvents(t, events)

	raw := newStdRegistrationConn(0)
	conn := newStdFDConn(events, raw)
	var reads atomic.Int32
	raw.readFunc = func(buffer []byte) (int, error) {
		if reads.Add(1) == 1 {
			// The bytes have arrived; the close and its OnClose run before
			// the read loop delivers them.
			_ = conn.Close()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Error("OnClose did not run")
			}
			return copy(buffer, "x"), nil
		}
		<-raw.closed
		return 0, net.ErrClosed
	}
	registerDone := make(chan error, 1)
	task := acquireTask(registerTask, conn)
	task.done = registerDone
	if !conn.loop.submitTask(task) {
		releaseTask(task)
		t.Fatal("register task was rejected")
	}
	if err := <-registerDone; err != nil {
		t.Fatalf("register error = %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("OnClose did not run")
	}
	// Serve joins the read loop, so its handling of the bytes is over.
	stopStdTestEvents(t, events, serveDone, errors.New("stop"))
	if dataAfterClose.Load() {
		t.Fatal("OnRead ran after OnClose")
	}
}

// OnWrite calls for one UDP connection never overlap, though its datagrams
// leave on the goroutines that write them: a sender that finds another one
// reporting leaves its bytes to that one and returns.
func TestStdUDPOnOutboundCallsDoNotOverlap(t *testing.T) {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := net.DialUDP("udp", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var in, overlap, calls atomic.Int32
	var reported atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	events := &Events{}
	funcs(events).BytesWritten = func(_ Conn, n int) {
		if in.Add(1) > 1 {
			overlap.Add(1)
		}
		reported.Add(int64(n))
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		in.Add(-1)
	}
	conn := &fdConn{udp: &stdUDPState{sock: client}}
	conn.entry = events.plainEntry()

	firstDone := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("first"))
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("OnWrite did not run for the first datagram")
	}
	if _, err := conn.Write([]byte("second!")); err != nil {
		t.Fatalf("second Write error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("OnWrite ran %d times while the first call was still running", got)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Write error = %v", err)
	}
	if got := reported.Load(); got != int64(len("first")+len("second!")) {
		t.Fatalf("OnWrite reported %d bytes, want %d", got, len("first")+len("second!"))
	}

	// Many senders at once: the reports never overlap and add up to what was sent.
	var sent atomic.Int64
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if n, err := conn.Write([]byte("burst")); err == nil {
					sent.Add(int64(n))
				}
			}
		}()
	}
	wg.Wait()
	if n := overlap.Load(); n != 0 {
		t.Fatalf("OnWrite overlapped %d times for one UDP connection", n)
	}
	if want := int64(len("first")+len("second!")) + sent.Load(); reported.Load() != want {
		t.Fatalf("OnWrite reported %d bytes, want %d", reported.Load(), want)
	}
}

// A UDP server's OnRead runs inside the peer's inbound callback scope, so the
// inbound methods work there, race-enabled builds included.
func TestStdUDPServerOnInboundHasInboundAccess(t *testing.T) {
	started := make(chan net.Addr, 1)
	got := make(chan string, 1)
	events := &Events{}
	funcs(events).Start = func(ev *Events) {
		for _, l := range ev.acceptor.listeners {
			if l.udp != nil {
				started <- l.udp.LocalAddr()
				return
			}
		}
	}
	funcs(events).Read = func(c Conn) error {
		buf := make([]byte, c.InboundBuffered())
		n, err := c.Read(buf)
		got <- string(buf[:n])
		return err
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("udp://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})

	var addr net.Addr
	select {
	case addr = <-started:
	case err := <-serveDone:
		t.Fatalf("Serve returned early: %v", err)
	}
	client, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("datagram")); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if s != "datagram" {
			t.Fatalf("OnRead read %q, want %q", s, "datagram")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnRead did not run for the datagram")
	}
}

// Two goroutines setting the read deadline at once leave the held wait and
// the socket agreeing: the first setter is stopped between its two updates
// while the second runs, and whichever value ends up recorded is the one the
// socket enforces once reads resume.
func TestStdHeldReadDeadlineAgreesWithTheSocket(t *testing.T) {
	const limit = 16 << 10
	var consume atomic.Bool
	closed := make(chan error, 1)
	events := withFuncs(&Events{
		Pollers:            1,
		MaxBufferSize:      4 << 10,
		MaxInboundBuffered: limit,
	}, FuncHandler{
		Read: func(c Conn) error {
			if consume.Load() {
				_, _ = c.Discard(-1)
			}
			return nil
		},
		Close: func(_ Conn, err error) { closed <- err },
	})
	client, server := serveInboundTest(t, events)
	writeUntilStalled(t, client, 16<<20)

	paused, release := make(chan struct{}), make(chan struct{})
	var stopped atomic.Bool
	readDeadlineNotedForTest = func() {
		// Only the first setter stops; sync.Once would make the second wait
		// for it, ordering the two setters itself.
		if stopped.CompareAndSwap(false, true) {
			close(paused)
			<-release
		}
	}
	t.Cleanup(func() { readDeadlineNotedForTest = nil })

	first := make(chan error, 1)
	go func() { first <- server.SetReadDeadline(time.Now().Add(100 * time.Millisecond)) }()
	<-paused
	second := make(chan error, 1)
	go func() { second <- server.SetReadDeadline(time.Time{}) }()
	time.Sleep(50 * time.Millisecond) // the second setter runs, or waits for the first
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}

	// Held, the recorded deadline decides; resumed, the socket's does. Either
	// both expire or neither does.
	expiredHeld := false
	select {
	case <-closed:
		expiredHeld = true
	case <-time.After(300 * time.Millisecond):
	}
	if expiredHeld {
		return
	}
	consume.Store(true)
	if err := server.Wake(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		t.Fatalf("the held wait had no deadline, but the resumed socket read expired: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// A deadline cleared while a held wait is deciding it has expired either
// loses, so the clear reports the connection closed, or wins, so the
// connection is not closed by the old deadline afterwards: once the clear
// returns success, the old deadline can no longer close the connection.
func TestStdHeldDeadlineExpiryAndClearAreOrdered(t *testing.T) {
	const limit = 16 << 10
	closed := make(chan error, 1)
	events := withFuncs(&Events{
		Pollers:            1,
		MaxBufferSize:      4 << 10,
		MaxInboundBuffered: limit,
	}, FuncHandler{
		Read:  func(Conn) error { return nil },
		Close: func(_ Conn, err error) { closed <- err },
	})
	client, server := serveInboundTest(t, events)
	writeUntilStalled(t, client, 16<<20)

	paused, release := make(chan struct{}), make(chan struct{})
	var stopped atomic.Bool
	heldDeadlineExpiredForTest = func() {
		if stopped.CompareAndSwap(false, true) {
			close(paused)
			<-release
		}
	}
	t.Cleanup(func() { heldDeadlineExpiredForTest = nil })

	if err := server.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-paused:
	case <-time.After(2 * time.Second):
		t.Fatal("the held wait never found its deadline passed")
	}
	cleared := make(chan error, 1)
	go func() { cleared <- server.SetReadDeadline(time.Time{}) }()
	time.Sleep(50 * time.Millisecond) // the clear returns, or waits for the expiry
	close(release)
	if err := <-cleared; err != nil {
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("clear error = %v, want success or net.ErrClosed", err)
		}
		return // the expiry came first, and the clear saw the connection closing
	}
	select {
	case err := <-closed:
		t.Fatalf("the clear succeeded, but the old deadline closed the connection: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// A read deadline the socket refuses leaves the held wait's deadline as it
// was: both keep the value last set successfully.
func TestStdRefusedReadDeadlineLeavesTheHeldWaitAlone(t *testing.T) {
	const limit = 16 << 10
	closed := make(chan error, 1)
	events := withFuncs(&Events{
		Pollers:            1,
		MaxBufferSize:      4 << 10,
		MaxInboundBuffered: limit,
	}, FuncHandler{
		Read:  func(Conn) error { return nil },
		Close: func(_ Conn, err error) { closed <- err },
	})
	client, server := serveInboundTest(t, events)
	writeUntilStalled(t, client, 16<<20)

	// Closing the socket underneath makes every setter fail while the
	// connection itself is not closing yet: the held wait reads no socket.
	_ = server.(*fdConn).conn.Close()
	if err := server.SetReadDeadline(time.Now().Add(-time.Second)); err == nil {
		t.Fatal("the socket accepted a deadline after being closed")
	}
	select {
	case err := <-closed:
		t.Fatalf("a deadline the socket refused closed the connection: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// Every Wake delivered before OnOpen has run gets its own OnRead after OnOpen,
// as a Wake does once the connection is open: none is merged into another.
// The Wakes are delivered first and the read goroutine's open runs after, the
// order a Dial that returns before its read goroutine starts allows.
func TestStdWakesBeforeOnOpenEachRunOnInbound(t *testing.T) {
	const wakes = 3
	var seq []string
	events := withFuncs(&Events{}, FuncHandler{
		Open: func(Conn) { seq = append(seq, "open") },
		Read: func(Conn) error { seq = append(seq, "data"); return nil },
	})
	conn := &fdConn{}
	conn.entry = events.plainEntry()
	for range wakes {
		events.callbackWG.Add(1)
		conn.deliverWake()
	}
	if len(seq) != 0 {
		t.Fatalf("callbacks ran before OnOpen: %v", seq)
	}
	if !conn.openOnReader() {
		t.Fatal("the connection did not open")
	}
	want := []string{"open", "data", "data", "data"}
	if fmt.Sprint(seq) != fmt.Sprint(want) {
		t.Fatalf("callbacks ran as %v, want %v", seq, want)
	}
}

// A connection torn down before its read goroutine ran, as a Dial racing a
// close leaves it, still gets OnOpen and then OnClose once, as on the native
// backend: the caller holding it learns of its end only through OnClose.
func TestStdCloseBeforeReaderStillOpensAndCloses(t *testing.T) {
	var seq []string
	cause := errors.New("closed before the reader ran")
	events := withFuncs(&Events{}, FuncHandler{
		Open:  func(Conn) { seq = append(seq, "open") },
		Close: func(_ Conn, err error) { seq = append(seq, "close: "+err.Error()) },
	})
	conn := &fdConn{}
	conn.entry = events.plainEntry()
	conn.closing.Store(true)
	conn.err = cause
	atomic.StoreInt32(&conn.closed, 1)
	conn.scheduleCloseCallback(cause)
	events.callbackWG.Wait()
	if len(seq) != 0 {
		t.Fatalf("callbacks ran before the reader: %v", seq)
	}
	if conn.openOnReader() {
		t.Fatal("a closed connection went on to read")
	}
	conn.scheduleCloseCallback(cause) // a late duplicate is dropped
	events.callbackWG.Wait()
	want := []string{"open", "close: " + cause.Error()}
	if fmt.Sprint(seq) != fmt.Sprint(want) {
		t.Fatalf("callbacks ran as %v, want %v", seq, want)
	}
}

// A DialContext canceled while the loop registers its connection returns the
// cause, and the connection it gave up gets neither OnOpen nor OnClose: the
// caller never had it.
func TestStdDialCanceledDuringRegistrationGetsNoCallbacks(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	var opens, closes atomic.Int32
	events := withFuncs(&Events{
		Pollers: 1,
	}, FuncHandler{
		Open:  func(Conn) { opens.Add(1) },
		Close: func(Conn, error) { closes.Add(1) },
	})
	serveDone := startStdTestEvents(t, events)
	cause := errors.New("dial canceled while registering")
	ctx, cancel := context.WithCancelCause(context.Background())
	returned := make(chan struct{})
	registeredForTest = func(*fdConn) {
		cancel(cause)
		<-returned
	}
	t.Cleanup(func() { registeredForTest = nil })
	conn, err := events.DialContext(ctx, "tcp://"+target.Addr().String(), nil)
	close(returned)
	if conn != nil || !errors.Is(err, cause) {
		t.Fatalf("DialContext = %v, %v; want nil, %v", conn, err, cause)
	}
	stopStdTestEvents(t, events, serveDone, errors.New("test done"))
	if o, c := opens.Load(), closes.Load(); o != 0 || c != 0 {
		t.Fatalf("the abandoned connection got %d OnOpen and %d OnClose, want none", o, c)
	}
}

// The acceptor hands each connection to its worker loop without waiting for
// the registration: a worker loop held up registering one connection does not
// stop connections bound for another loop from being accepted and opened.
func TestStdAcceptDoesNotWaitForRegistration(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 3 {
		t.Skip("needs three worker loops, and Pollers is capped at GOMAXPROCS")
	}
	release := make(chan struct{})
	var held atomic.Pointer[eventLoop]
	registeredForTest = func(conn *fdConn) {
		if held.CompareAndSwap(nil, conn.loop) {
			<-release
		}
	}
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		releaseOnce()
		registeredForTest = nil
	})
	opened := make(chan struct{}, 16)
	// Three loops: the client sockets dialed here take descriptors too, so
	// the accepted ones step by two, which a pair of loops would map to one.
	events := withFuncs(&Events{Pollers: 3}, FuncHandler{Open: func(Conn) { opened <- struct{}{} }})
	addr, done := serveLoopback(t, events)
	t.Cleanup(func() {
		releaseOnce()
		_ = events.Close(nil)
		<-done
	})
	var clients []net.Conn
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	// The first registration blocks its loop; the rest of the dials reach
	// the other loops too, since loops are picked by descriptor number.
	for range 6 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("no connection opened while one worker loop was registering")
	}
}

// stdioBackend tells cross-backend tests which backend they run on.
const stdioBackend = true

// TestStdWakeThenDialDoesNotStall pins that Wake callbacks are delivered
// outside the event loop: with one loop, a callback that wakes and then dials
// synchronously must not cycle with registration. When the loop ran wake
// callbacks itself, it waited on the connection's callback mutex — held by the
// running read callback — while that callback waited for the loop to register
// the dialed connection, and neither side could advance.
func TestStdWakeThenDialDoesNotStall(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		peer, err := target.Accept()
		if err == nil {
			defer peer.Close()
			var probe [1]byte
			_, _ = peer.Read(probe[:])
		}
	}()

	ev := &Events{Pollers: 1}
	started := make(chan struct{})
	result := make(chan error, 1)
	var calls atomic.Int32
	funcs(ev).Start = func(*Events) { close(started) }
	funcs(ev).Read = func(conn Conn) error {
		if calls.Add(1) != 1 {
			return nil
		}
		_, _ = conn.Discard(-1)
		if err := conn.Wake(); err != nil {
			result <- err
			return err
		}
		// Give the loop time to take the wake before dialing: the loop must
		// not be waiting on this callback's mutex while Dial blocks on it.
		time.Sleep(50 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		dialed, err := ev.DialContext(ctx, target.Addr().String(), nil)
		if dialed != nil {
			_ = dialed.Close()
		}
		result <- err
		return nil
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- ev.Serve() }()
	<-started
	t.Cleanup(func() {
		_ = ev.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})

	client, server := tcpConnectionPair(t)
	defer client.Close()
	if _, err := ev.Adopt(server, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("DialContext from OnRead stalled behind queued Wake: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnRead failed to return")
	}
}

// A datagram the listener read just before its server closed is handled after
// closeOnLoop set peers to nil: the lookup on the nil map misses, and the
// insertion must be skipped rather than panic.
func TestStdUDPListenerCloseDuringDatagramFlood(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		started := make(chan net.Addr, 1)
		events := &Events{}
		funcs(events).Start = func(ev *Events) {
			for _, l := range ev.acceptor.listeners {
				if l.udp != nil {
					started <- l.udp.LocalAddr()
					return
				}
			}
		}
		funcs(events).Read = func(c Conn) error { return nil }
		serveDone := make(chan error, 1)
		go func() { serveDone <- events.Serve("udp://127.0.0.1:0") }()
		addr := <-started
		var stop atomic.Bool
		floodDone := make(chan struct{})
		go func() {
			defer close(floodDone)
			client, err := net.Dial("udp", addr.String())
			if err != nil {
				return
			}
			defer client.Close()
			for !stop.Load() {
				_, _ = client.Write([]byte("x"))
			}
		}()
		time.Sleep(2 * time.Millisecond)
		_ = events.Close(nil)
		<-serveDone
		stop.Store(true)
		<-floodDone
	}
}

// A UDP child published while its server was still live but opened after the
// server closed gets its OnClose: the listener re-checks instead of leaving
// the child opened and undelivered.
func TestStdUDPChildClosedBeforeOpenGetsOnClose(t *testing.T) {
	started := make(chan net.Addr, 1)
	created := make(chan *fdConn, 1)
	release := make(chan struct{})
	testHookUDPChildCreated = func(c *fdConn) {
		created <- c
		<-release
	}
	defer func() { testHookUDPChildCreated = nil }()
	var opens, closes atomic.Int32
	events := &Events{}
	funcs(events).Start = func(ev *Events) {
		for _, l := range ev.acceptor.listeners {
			if l.udp != nil {
				started <- l.udp.LocalAddr()
				return
			}
		}
	}
	funcs(events).Open = func(c Conn) { opens.Add(1) }
	funcs(events).Close = func(c Conn, err error) { closes.Add(1) }
	funcs(events).Read = func(c Conn) error { return nil }
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("udp://127.0.0.1:0") }()
	addr := <-started
	client, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = client.Write([]byte("x"))
	child := <-created
	_ = events.Close(nil)
	for !child.IsClosed() {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the OnClose goroutine pass on the child
	close(release)
	select {
	case <-serveDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	if opens.Load() != closes.Load() {
		t.Fatalf("UDP child got %d OnOpen and %d OnClose", opens.Load(), closes.Load())
	}
}

// The close reason publishes before the closed flag, so a child that observes
// itself closed delivers the reason, not a nil.
func TestStdUDPChildCloseReasonPublished(t *testing.T) {
	started := make(chan net.Addr, 1)
	closed := make(chan error, 1)
	events := &Events{}
	funcs(events).Start = func(e *Events) {
		for _, l := range e.acceptor.listeners {
			if l.udp != nil {
				started <- l.udp.LocalAddr()
				return
			}
		}
	}
	funcs(events).Open = func(c Conn) {
		_ = c.CloseWith(errors.New("test-close"))
		for !c.(*fdConn).IsClosed() {
			runtime.Gosched()
		}
	}
	funcs(events).Close = func(c Conn, err error) { closed <- err }
	done := make(chan error, 1)
	go func() { done <- events.Serve("udp://127.0.0.1:0") }()
	client, err := net.Dial("udp", (<-started).String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = client.Write([]byte("x"))
	select {
	case err := <-closed:
		if err == nil || err.Error() != "test-close" {
			t.Fatalf("close reason = %v, want test-close", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close missing")
	}
	_ = events.Close(nil)
	<-done
}

// A Control that starts while Listener.Close is closing the socket — after
// Close found no Control running — is refused, and a Close made inside it
// cannot wait on the first: the check and the refusal are one step, so the
// socket's close never waits for a Control that waits for it.
func TestUDPControlDuringListenerCloseIsRefused(t *testing.T) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, Hooks: FuncHandler{Start: func(*Events) { close(started) }}}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	listener, err := events.Listen("udp4://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := listener.(PacketListener).DialPeer(netip.MustParseAddrPort("127.0.0.1:9001"), nil)
	if err != nil {
		t.Fatal(err)
	}
	server := listener.(packetListener).group.members[0].udpSvr
	server.mux.Lock() // hold the first Close inside its socket close
	firstDone := make(chan struct{})
	go func() { _ = listener.Close(); close(firstDone) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		stacks := string(buf[:runtime.Stack(buf, true)])
		if strings.Contains(stacks, "(*fdConn).closeOnLoop") && strings.Contains(stacks, "(*fdConn).closeListenerSocket") {
			break
		}
		if time.Now().After(deadline) {
			server.mux.Unlock()
			t.Fatal("the first Close never reached the socket's close")
		}
		runtime.Gosched()
	}
	controlled := make(chan error, 1)
	go func() {
		controlled <- peer.Control(func(uintptr) { _ = listener.Close() })
	}()
	select {
	case err := <-controlled:
		if !errors.Is(err, net.ErrClosed) {
			server.mux.Unlock()
			t.Fatalf("a Control during the close = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		server.mux.Unlock()
		t.Fatal("a Control during the close neither ran nor was refused")
	}
	server.mux.Unlock()
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the first Close never finished")
	}
}

// AdoptContext answers its context while the connection waits to register:
// with the loop held up by another connection's close, a deadline ends the
// wait at once; the adopted connection is closed once the loop gets to it,
// and no callback runs for it.
func TestAdoptContextCanceledWhileRegistering(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	events := &Events{Pollers: 1, Hooks: FuncHandler{Start: func(*Events) { close(started) }}}
	count := func(c Conn) { // the canceled adopt's callbacks, not the first one's
		if c.Userdata() == "canceled" {
			calls.Add(1)
		}
	}
	events.Handler = FuncHandler{Open: count, Close: func(c Conn, _ error) { count(c) }}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started

	pair := func() (net.Conn, net.Conn) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		remote, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		local, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = remote.Close() })
		return local, remote
	}
	local, _ := pair()
	blocker := &closeBlocker{Conn: local, entered: make(chan struct{}), release: make(chan struct{})}
	first, err := events.Adopt(blocker, nil, WithLayers(PassThroughLayer))
	if err != nil {
		t.Fatal(err)
	}
	blocker.block.Store(true)
	_ = first.CloseWith(errors.New("abort"))
	<-blocker.entered // the one loop is held in this close

	second, peer := pair()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	adopted := make(chan error, 1)
	go func() {
		_, err := events.AdoptContext(ctx, second, "canceled", WithLayers(PassThroughLayer))
		adopted <- err
	}()
	var adoptErr error
	select {
	case adoptErr = <-adopted:
	case <-time.After(500 * time.Millisecond):
		close(blocker.release)
		<-adopted
		t.Fatal("AdoptContext went on waiting past its deadline for the loop")
	}
	close(blocker.release) // the loop moves on, and closes what the adopt left
	if !errors.Is(adoptErr, context.DeadlineExceeded) {
		t.Fatalf("AdoptContext = %v, want the deadline", adoptErr)
	}
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the connection the canceled adopt took was not closed: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("a canceled adopt ran %d callbacks", n)
	}
}

// closeBlocker holds its Close, once told to, until released.
type closeBlocker struct {
	net.Conn
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *closeBlocker) SyscallConn() (syscall.RawConn, error) {
	return c.Conn.(syscall.Conn).SyscallConn()
}

func (c *closeBlocker) Close() error {
	if c.block.Load() {
		c.once.Do(func() { close(c.entered) })
		<-c.release
	}
	return c.Conn.Close()
}
