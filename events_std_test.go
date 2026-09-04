//go:build windows || stdio

package uio

import (
	"bytes"
	"errors"
	"io"
	"net"
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
	events.OnStart = func(*Events) { close(started) }
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
		conn: raw, writeSig: make(chan struct{}, 1), closeSig: make(chan struct{}),
	}
	conn.events = events
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
	events.OnOpen = func(conn Conn) {
		raw := conn.(*fdConn).conn.(*stdRegistrationConn)
		close(openEntered)
		<-releaseOpen
		if _, err := conn.Write([]byte("x")); err != nil {
			callbackErr <- err
		}
		raw.openReturned.Store(true)
	}
	events.OnData = func(conn Conn) error {
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
		t.Fatal("OnData was not called")
	}
	if raw.orderViolation.Load() {
		t.Fatal("I/O callback ran before OnOpen returned")
	}
	if raw.maxReadActive.Load() != 1 || raw.writeCalls.Load() != 1 {
		t.Fatalf("connection started duplicate I/O: concurrent reads %d, writes %d", raw.maxReadActive.Load(), raw.writeCalls.Load())
	}

	stopStdTestEvents(t, events, serveDone, errors.New("ordered shutdown"))
}

func TestStdRegisterBurstAndClose(t *testing.T) {
	const desiredTotal = 2048
	total := min(desiredTotal, stdTestFDLimit())
	events := &Events{Pollers: 1, MaxBufferSize: 64}
	var opened atomic.Int32
	events.OnOpen = func(conn Conn) {
		conn.(*fdConn).conn.(*stdRegistrationConn).openReturned.Store(true)
		opened.Add(1)
	}
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
	if got := int(opened.Load()); got != total {
		t.Fatalf("OnOpen called %d times, want %d", got, total)
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
	events.OnOpen = func(conn Conn) {
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
	closeDone := make(chan error, 1)
	go func() { closeDone <- events.Close(shutdownErr) }()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-raw.closed:
	case <-timer.C:
		t.Fatal("Close did not interrupt the blocking socket write")
	}
	select {
	case <-closeDone:
		t.Fatal("Close returned before the blocked writer released its payload")
	default:
	}
	close(raw.writeRelease)

	for closeDone != nil || serveDone != nil {
		select {
		case err := <-closeDone:
			if err != nil {
				t.Fatalf("Events.Close error = %v", err)
			}
			closeDone = nil
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
		commonConn: commonConn{events: &Events{MaxOutboundBuffered: -1}},
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
			commonConn: commonConn{events: &Events{MaxOutboundBuffered: -1}},
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
			commonConn: commonConn{events: &Events{MaxOutboundBuffered: -1}},
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
			commonConn: commonConn{events: &Events{MaxOutboundBuffered: -1}},
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
		commonConn: commonConn{events: &Events{MaxOutboundBuffered: -1}},
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
				commonConn: commonConn{events: &Events{MaxOutboundBuffered: -1}},
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
		commonConn: commonConn{events: &Events{MaxOutboundBuffered: -1}},
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
		commonConn: commonConn{events: &Events{MaxOutboundBuffered: 16}},
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
	conn := &fdConn{udp: sender}
	conn.events = &Events{}

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
		conn: raw, writeSig: make(chan struct{}, 1), closeSig: make(chan struct{}),
	}
	conn.events = &Events{MaxOutboundBuffered: 3}
	writeLoopDone := make(chan struct{})
	go func() {
		conn.writeLoop()
		close(writeLoopDone)
	}()
	defer func() {
		close(conn.closeSig)
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

func TestStdFlushCoalescesWakeWithoutAllocating(t *testing.T) {
	conn := &fdConn{writeSig: make(chan struct{}, 1)}
	conn.events = &Events{}

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
	events := &Events{Pollers: 1}
	events.OnStart = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.ln.Addr().String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	events.OnOpen = func(conn Conn) { opened <- conn }
	events.OnData = func(conn Conn) error {
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
		t.Fatal("stdio Wake did not invoke OnData")
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

func TestStdEventsCloseFromOnDataDoesNotDeadlock(t *testing.T) {
	started := make(chan string, 1)
	closeReturned := make(chan struct{})
	shutdownErr := errors.New("stdio callback shutdown")
	events := &Events{Pollers: 1}
	events.OnStart = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.ln.Addr().String()
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
	client, err := net.Dial("tcp", <-started)
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
		t.Fatal("Events.Close blocked in stdio OnData")
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

func TestStdExternalCloseWaitsForLifetimeCallback(t *testing.T) {
	started := make(chan string, 1)
	callbackEntered := make(chan bool, 1)
	releaseCallback := make(chan struct{})
	shutdownErr := errors.New("external close during std callback")
	var callbackOnce sync.Once

	events := &Events{Pollers: 1}
	events.OnStart = func(events *Events) {
		events.acceptor.mux.Lock()
		defer events.acceptor.mux.Unlock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.ln.Addr().String()
			return
		}
	}
	events.OnData = func(conn Conn) error {
		callbackOnce.Do(func() {
			_, registered := events.callbackGoids.Load(currentGoroutineID())
			callbackEntered <- registered
			<-releaseCallback
		})
		_, _ = conn.Discard(-1)
		return nil
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	client, err := net.Dial("tcp", <-started)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Write([]byte("block")); err != nil {
		t.Fatal(err)
	}
	if registered := <-callbackEntered; !registered {
		t.Fatal("read goroutine was not registered for callback-safe Close")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- events.Close(shutdownErr) }()
	select {
	case <-closeDone:
		t.Fatal("external Events.Close returned while OnData was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseCallback)

	select {
	case err = <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("external Events.Close did not finish after OnData returned")
	}
	select {
	case err = <-serveDone:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop")
	}

	entries := 0
	events.callbackGoids.Range(func(_, _ any) bool {
		entries++
		return true
	})
	if entries != 0 {
		t.Fatalf("callback goroutine entries after shutdown = %d", entries)
	}
}

func TestStdReadLoopKeepsCallbackRegistrationBetweenReads(t *testing.T) {
	raw := newStdRegistrationConn(30004)
	raw.openReturned.Store(true)
	raw.secondRead = make(chan struct{})
	events := &Events{MaxBufferSize: 64}
	callbackID := make(chan int64, 1)
	events.OnData = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		callbackID <- currentGoroutineID()
		return nil
	}
	conn := &fdConn{conn: raw}
	conn.events = events
	readDone := make(chan struct{})
	go func() {
		conn.readLoop()
		close(readDone)
	}()

	raw.readData <- []byte("first")
	id := <-callbackID
	select {
	case <-raw.secondRead:
	case <-time.After(time.Second):
		t.Fatal("read loop did not start its second read")
	}
	if _, registered := events.callbackGoids.Load(id); !registered {
		t.Fatal("callback goroutine was unregistered between reads")
	}

	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("read loop did not exit")
	}
	if _, registered := events.callbackGoids.Load(id); registered {
		t.Fatal("callback goroutine remained registered after read loop exit")
	}
}
