//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// readPeerUntil reads exactly size bytes from a non-blocking peer descriptor,
// failing once timeout passes without completing.
func readPeerUntil(t *testing.T, fd int, size int, timeout time.Duration) []byte {
	t.Helper()
	result := make([]byte, size)
	offset := 0
	deadline := time.Now().Add(timeout)
	for offset < size {
		if time.Now().After(deadline) {
			t.Fatalf("read %d bytes, want %d", offset, size)
		}
		n, err := unix.Read(fd, result[offset:])
		if n > 0 {
			offset += n
		}
		if err == nil {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			time.Sleep(100 * time.Microsecond)
			continue
		}
		t.Fatal(err)
	}
	return result
}

// writePeerAll writes data to a non-blocking peer descriptor.
func writePeerAll(fd int, data []byte, deadline time.Time) error {
	for len(data) > 0 {
		if time.Now().After(deadline) {
			return errors.New("peer write timed out")
		}
		n, err := unix.Write(fd, data)
		if n > 0 {
			data = data[n:]
		}
		if err == nil {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			time.Sleep(100 * time.Microsecond)
			continue
		}
		return err
	}
	return nil
}

// A write from another goroutine is sent by a write turn while the
// connection's own turn is still inside a callback.
func TestWriteTurnSendsBesideBlockedCallback(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	events := &Events{Pollers: 1}
	events.OnData = func(conn Conn) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		_, _ = conn.Discard(-1)
		return nil
	}
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("callback was not entered")
	}
	if _, err := testConn.conn.Write([]byte("beside")); err != nil {
		t.Fatal(err)
	}
	if got := string(readPeerUntil(t, testConn.peer, 6, 2*time.Second)); got != "beside" {
		t.Fatalf("peer received %q while the callback was blocked", got)
	}
}

// Concurrent producers keep their own order, and nothing is lost or repeated.
func TestWriteTurnsPreserveProducerOrder(t *testing.T) {
	const producers, records, recordSize = 8, 2000, 8
	events := &Events{Pollers: 1}
	testConn := newTestConnection(t, events)
	var wg sync.WaitGroup
	for producer := 0; producer < producers; producer++ {
		wg.Add(1)
		go func(producer byte) {
			defer wg.Done()
			var record [recordSize]byte
			for seq := uint32(0); seq < records; seq++ {
				record[0] = producer
				binary.LittleEndian.PutUint32(record[1:], seq)
				record[5] = producer ^ byte(seq)
				if _, err := testConn.conn.Write(record[:]); err != nil {
					t.Error(err)
					return
				}
			}
		}(byte(producer))
	}
	data := readPeerUntil(t, testConn.peer, producers*records*recordSize, 10*time.Second)
	wg.Wait()
	next := make([]uint32, producers)
	for offset := 0; offset < len(data); offset += recordSize {
		record := data[offset : offset+recordSize]
		producer, seq := record[0], binary.LittleEndian.Uint32(record[1:])
		if int(producer) >= producers || record[5] != producer^byte(seq) {
			t.Fatalf("corrupt record at %d: %v", offset, record)
		}
		if seq != next[producer] {
			t.Fatalf("producer %d sent seq %d, want %d", producer, seq, next[producer])
		}
		next[producer]++
	}
}

// A sender that finds nothing left to send releases its claim while a producer
// may be appending: the producer's kick finds the claim taken, so the release
// must look again. Many short bursts give the last write of each burst that
// chance; a lost one stays queued and the burst never arrives.
func TestWriteTurnReleaseLosesNoWrite(t *testing.T) {
	const rounds, producers = 2000, 4
	testConn := newTestConnection(t, &Events{Pollers: 1})
	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		for producer := range producers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := testConn.conn.Write([]byte{byte(producer)}); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		readPeerUntil(t, testConn.peer, producers, 2*time.Second)
	}
}

// A callback's direct send outside a read round takes the write claim like
// every other sender, so it neither overlaps a write turn's send nor gives
// away a claim it does not hold: both producers' records arrive whole, once,
// and in order.
func TestDirectSendSharesTheWriteClaim(t *testing.T) {
	const records, recordSize = 4000, 8
	testConn := newTestConnection(t, &Events{Pollers: 1})
	conn := testConn.conn
	for deadline := time.Now().Add(2 * time.Second); conn.writeState.Load()&writeOpenedFlag == 0 ||
		conn.ioOwner.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("connection did not finish opening")
		}
		time.Sleep(time.Millisecond)
	}
	record := func(tag byte, seq uint32) []byte {
		var r [recordSize]byte
		r[0] = tag
		binary.LittleEndian.PutUint32(r[1:], seq)
		r[5] = tag ^ byte(seq)
		return r[:]
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for seq := uint32(0); seq < records; seq++ {
			if _, err := conn.Write(record('W', seq)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		// This goroutine stands in for the idle connection's own callback,
		// whose uncorked writes take the direct path.
		conn.ioOwner.Store(currentGoroutineID())
		defer conn.ioOwner.Store(0)
		for seq := uint32(0); seq < records; seq++ {
			if _, err := conn.Write(record('D', seq)); err != nil {
				t.Error(err)
				return
			}
		}
		// Bytes the owner queued behind a write turn leave with its release.
		if _, err := conn.flushOnLoop(); err != nil {
			t.Error(err)
		}
	}()
	data := readPeerUntil(t, testConn.peer, 2*records*recordSize, 10*time.Second)
	wg.Wait()
	next := map[byte]uint32{}
	for offset := 0; offset < len(data); offset += recordSize {
		r := data[offset : offset+recordSize]
		tag, seq := r[0], binary.LittleEndian.Uint32(r[1:])
		if (tag != 'W' && tag != 'D') || r[5] != tag^byte(seq) {
			t.Fatalf("corrupt record at %d: %v", offset, r)
		}
		if seq != next[tag] {
			t.Fatalf("%c record seq %d, want %d", tag, seq, next[tag])
		}
		next[tag]++
	}
}

// Bytes handed out by ReserveOutbound are filled by the callback after the
// reservation returns. A write turn sending at the same time must never take
// them first, so every record the peer sees is complete.
func TestReserveOutboundIsNeverSentUnfilled(t *testing.T) {
	const triggers, producerRecords, recordSize = 4000, 4000, 8
	var reserved, fallback atomic.Int64
	var replySeq uint32
	events := &Events{Pollers: 1}
	events.OnData = func(conn Conn) error {
		n := conn.InboundBuffered()
		_, _ = conn.Discard(n)
		for range n {
			seq := replySeq
			replySeq++
			dst, err := conn.ReserveOutbound(recordSize)
			if errors.Is(err, ErrReserveUnsupported) {
				fallback.Add(1)
				var record [recordSize]byte
				record[0] = 'R'
				binary.LittleEndian.PutUint32(record[1:], seq)
				record[5] = 'R' ^ byte(seq)
				if _, err = conn.Write(record[:]); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			reserved.Add(1)
			// Filled only now, after the reservation returned, and slowly,
			// so a sender that took the reservation early would catch it
			// half written.
			dst[0] = 'R'
			for deadline := time.Now().Add(2 * time.Microsecond); time.Now().Before(deadline); {
			}
			binary.LittleEndian.PutUint32(dst[1:], seq)
			dst[5] = 'R' ^ byte(seq)
			dst[6], dst[7] = 0, 0
		}
		return nil
	}
	testConn := newTestConnection(t, events)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var record [recordSize]byte
		for seq := uint32(0); seq < producerRecords; seq++ {
			record[0] = 'W'
			binary.LittleEndian.PutUint32(record[1:], seq)
			record[5] = 'W' ^ byte(seq)
			if _, err := testConn.conn.Write(record[:]); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		trigger := bytes.Repeat([]byte{1}, 16)
		for sent := 0; sent < triggers; sent += len(trigger) {
			if err := writePeerAll(testConn.peer, trigger, time.Now().Add(5*time.Second)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	data := readPeerUntil(t, testConn.peer, (triggers+producerRecords)*recordSize, 10*time.Second)
	wg.Wait()
	next := map[byte]uint32{}
	for offset := 0; offset < len(data); offset += recordSize {
		record := data[offset : offset+recordSize]
		tag, seq := record[0], binary.LittleEndian.Uint32(record[1:])
		if (tag != 'R' && tag != 'W') || record[5] != tag^byte(seq) {
			t.Fatalf("corrupt record at %d: %v", offset, record)
		}
		if seq != next[tag] {
			t.Fatalf("%c record seq %d, want %d", tag, seq, next[tag])
		}
		next[tag]++
	}
	t.Logf("reservations=%d fallbacks=%d", reserved.Load(), fallback.Load())
}

// With an outbound limit, reads pause while replies queued by another
// goroutine pile up behind a slow peer, and resume once the blocked socket
// drains.
func TestWriteTurnResumesReadsAfterBlockedSend(t *testing.T) {
	const total = 1 << 20
	replies := make(chan []byte, 1024)
	events := &Events{Pollers: 1, MaxOutboundBuffered: 16 << 10, MaxBufferSize: 4 << 10}
	events.OnData = func(conn Conn) error {
		buf := make([]byte, conn.InboundBuffered())
		_, _ = io.ReadFull(conn, buf)
		replies <- buf
		return nil
	}
	testConn := newTestConnection(t, events)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case reply := <-replies:
				for {
					_, err := testConn.conn.Write(reply)
					if err == nil {
						break
					}
					if !errors.Is(err, ErrOutboundOverflow) {
						return
					}
					time.Sleep(50 * time.Microsecond)
				}
			}
		}
	}()
	input := make([]byte, total)
	for i := range input {
		input[i] = byte(i * 7)
	}
	writeErr := make(chan error, 1)
	go func() { writeErr <- writePeerAll(testConn.peer, input, time.Now().Add(20*time.Second)) }()
	// Let the limit fill and pause reads before the peer drains.
	time.Sleep(100 * time.Millisecond)
	echoed := readPeerUntil(t, testConn.peer, total, 20*time.Second)
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(echoed, input) {
		t.Fatal("echoed stream differs from the input")
	}
}

// Over loopback TCP the socket rarely refuses a send, so the paused reads have
// no writable edge to restart them: the write turn that retires the backlog to
// the resume mark must restart them itself.
func TestWriteTurnResumesReadsUnderOutboundLimit(t *testing.T) {
	const total = 8 << 20
	replies := make(chan []byte, 4096)
	started := make(chan string, 1)
	opened := make(chan Conn, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: 8 << 10}
	events.OnStart = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.laddr.String()
			return
		}
	}
	events.OnOpen = func(conn Conn) { opened <- conn }
	events.OnData = func(conn Conn) error {
		buf := make([]byte, conn.InboundBuffered())
		_, _ = io.ReadFull(conn, buf)
		replies <- buf
		return nil
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	client, err := net.Dial("tcp", <-started)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-opened
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case reply := <-replies:
				for {
					_, err := server.Write(reply)
					if err == nil {
						break
					}
					if !errors.Is(err, ErrOutboundOverflow) {
						return
					}
					time.Sleep(20 * time.Microsecond)
				}
			}
		}
	}()
	input := make([]byte, total)
	for i := range input {
		input[i] = byte(i * 13)
	}
	go func() { _, _ = client.Write(input) }()
	_ = client.SetReadDeadline(time.Now().Add(20 * time.Second))
	echoed := make([]byte, total)
	if n, err := io.ReadFull(client, echoed); err != nil {
		t.Fatalf("echoed %d of %d bytes: %v", n, total, err)
	}
	if !bytes.Equal(echoed, input) {
		t.Fatal("echoed stream differs from the input")
	}
}

// A read that finds the outbound limit full while a write turn holds the claim
// pauses: it is not redelivered while the backlog stays high, and the write
// turn that drains the backlog restarts it without any writable edge.
func TestOutboundLimitPausesReadsUntilWriteTurnDrains(t *testing.T) {
	const limit, readSize, sent = 1024, 64, 200
	var armed atomic.Bool
	var consumed atomic.Int64
	holding := make(chan struct{})
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	release := func() { unblockOnce.Do(func() { close(unblock) }) }
	defer release()
	events := &Events{Pollers: 1, MaxOutboundBuffered: limit, MaxBufferSize: readSize}
	events.OnOutbound = func(Conn, int) {
		if armed.CompareAndSwap(true, false) {
			close(holding)
			<-unblock
		}
	}
	events.OnData = func(conn Conn) error {
		n, _ := conn.Discard(-1)
		consumed.Add(int64(n))
		return nil
	}
	testConn := newTestConnection(t, events)
	// Start from an open, idle connection, so the open turn neither sends the
	// first byte itself nor sees the filled limit.
	for deadline := time.Now().Add(2 * time.Second); testConn.conn.writeState.Load()&writeOpenedFlag == 0 ||
		testConn.conn.ioOwner.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("connection did not finish opening")
		}
		time.Sleep(time.Millisecond)
	}
	waitConsumed := func(want int64, failure string) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); consumed.Load() < want; {
			if time.Now().After(deadline) {
				t.Fatalf("%s: consumed %d of %d bytes", failure, consumed.Load(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// A write turn sends one byte, then keeps its claim inside OnOutbound.
	armed.Store(true)
	if _, err := testConn.conn.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-holding:
	case <-time.After(2 * time.Second):
		t.Fatal("write turn did not start")
	}
	// The limit fills behind the held claim.
	if _, err := testConn.conn.Write(bytes.Repeat([]byte{'y'}, limit)); err != nil {
		t.Fatal(err)
	}
	// The peer sends more than one read takes; the first read finds the
	// limit full and pauses.
	if err := writePeerAll(testConn.peer, make([]byte, sent), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	waitConsumed(readSize, "the first read did not happen")
	time.Sleep(50 * time.Millisecond)
	if n := consumed.Load(); n != readSize {
		t.Fatalf("reads went on while the outbound limit was full: consumed %d bytes", n)
	}
	// Draining the backlog never fills the socket, so no writable edge comes.
	release()
	waitConsumed(sent, "paused reads did not resume after the write turn drained the backlog")
	if got := readPeerUntil(t, testConn.peer, 1+limit, 2*time.Second); got[0] != 'x' ||
		!bytes.Equal(got[1:], bytes.Repeat([]byte{'y'}, limit)) {
		t.Fatal("peer received a different stream")
	}
}

// A writable edge reaches the connection's turn while another sender may sit
// between its EAGAIN and recording it; that edge will not come again. The
// sender then retries instead of blocking, and an edge after the record
// clears it, so the holder's release sends again.
func TestWritableEdgeReachesABusySender(t *testing.T) {
	var conn fdConn
	conn.noteWritable()
	if !conn.markWriteBlocked() || conn.writeBlocked() {
		t.Fatal("an EAGAIN after a handled edge blocked instead of retrying")
	}
	if conn.markWriteBlocked() || !conn.writeBlocked() {
		t.Fatal("an EAGAIN with no edge since did not block")
	}
	conn.noteWritable()
	if conn.writeBlocked() {
		t.Fatal("a writable edge left the socket blocked")
	}
}

// Closing while write turns are sending must never write a closed descriptor:
// its number is reused at once by the next socket of this process, which may
// be either end of the next connection. Neither client ever sends, so any byte
// the server receives, or connection B's client reads, came from A's senders.
func TestCloseNeverWritesReusedDescriptor(t *testing.T) {
	opened := make(chan *fdConn, 4)
	closed := make(chan *fdConn, 4)
	started := make(chan string, 1)
	var strayInbound atomic.Int64
	events := &Events{Pollers: 1}
	events.OnStart = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.laddr.String()
			return
		}
	}
	events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn) }
	events.OnData = func(conn Conn) error {
		n, _ := conn.Discard(-1)
		strayInbound.Add(int64(n))
		return nil
	}
	events.OnClose = func(conn Conn, _ error) { closed <- conn.(*fdConn) }
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	var addr string
	select {
	case addr = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not start")
	}
	reused := 0
	for iteration := 0; iteration < 30; iteration++ {
		clientA, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		serverA := <-opened
		fdA := serverA.fd
		go func() { _, _ = io.Copy(io.Discard, clientA) }()
		var producers sync.WaitGroup
		chunk := bytes.Repeat([]byte{'A'}, 1024)
		for range 4 {
			producers.Add(1)
			go func() {
				defer producers.Done()
				for {
					if _, err := serverA.Write(chunk); err != nil {
						return
					}
				}
			}()
		}
		time.Sleep(2 * time.Millisecond)
		_ = serverA.CloseWith(io.EOF)
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("OnClose was not delivered")
		}
		clientB, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		serverB := <-opened
		if serverB.fd == fdA {
			reused++
		}
		_ = clientB.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		var probe [1]byte
		if n, _ := clientB.Read(probe[:]); n > 0 {
			t.Fatalf("connection B received %q written for A on descriptor %d", probe[:n], fdA)
		}
		producers.Wait()
		_ = clientA.Close()
		_ = serverB.CloseWith(io.EOF)
		<-closed
		_ = clientB.Close()
		if n := strayInbound.Load(); n != 0 {
			t.Fatalf("server received %d bytes no client sent", n)
		}
	}
	t.Logf("descriptor reused by the accepted end in %d of 30 iterations", reused)
}

// Output queued right after Dial, before OnOpen has returned, is sent after
// OnOpen: write turns never run ahead of the open callback.
func TestWriteBeforeOpenWaitsForOnOpen(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan []byte, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		buf := make([]byte, 5)
		_, _ = io.ReadFull(peer, buf)
		received <- buf
	}()
	var openReturned, earlySend atomic.Bool
	started := make(chan struct{}, 1)
	events := &Events{Pollers: 1}
	events.OnStart = func(*Events) { started <- struct{}{} }
	events.OnOpen = func(Conn) {
		time.Sleep(50 * time.Millisecond)
		openReturned.Store(true)
	}
	events.OnOutbound = func(Conn, int) {
		if !openReturned.Load() {
			earlySend.Store(true)
		}
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Events did not start")
	}
	conn, err := events.Dial("tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if openReturned.Load() {
		t.Skip("OnOpen returned before Dial did")
	}
	if _, err = conn.Write([]byte("early")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if string(got) != "early" {
			t.Fatalf("peer received %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("output queued before OnOpen was never sent")
	}
	if earlySend.Load() {
		t.Fatal("output was sent before OnOpen returned")
	}
}

// Serve returns only after every write turn has finished: no OnOutbound runs
// afterwards.
func TestServeJoinsWriteTurns(t *testing.T) {
	opened := make(chan Conn, 1)
	started := make(chan string, 1)
	var stopped atomic.Bool
	var lateOutbound atomic.Int64
	events := &Events{Pollers: 1}
	events.OnStart = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.laddr.String()
			return
		}
	}
	events.OnOpen = func(conn Conn) { opened <- conn }
	events.OnOutbound = func(Conn, int) {
		if stopped.Load() {
			lateOutbound.Add(1)
		}
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	addr := <-started
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	go func() { _, _ = io.Copy(io.Discard, client) }()
	server := <-opened
	var producers sync.WaitGroup
	chunk := bytes.Repeat([]byte{'s'}, 4096)
	for range 4 {
		producers.Add(1)
		go func() {
			defer producers.Done()
			for {
				if _, err := server.Write(chunk); err != nil {
					return
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	_ = events.Close(nil)
	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	stopped.Store(true)
	producers.Wait()
	time.Sleep(20 * time.Millisecond)
	if n := lateOutbound.Load(); n != 0 {
		t.Fatalf("%d OnOutbound calls after Serve returned", n)
	}
}
