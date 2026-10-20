//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/socket"
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

// A direct send takes the write claim like every other sender, so it neither
// overlaps a write turn's send nor gives away a claim it does not hold: both
// producers' records arrive whole, once, and in order. A goroutine stands in
// for the connection's own uncorked callback. The socket buffers hold every
// record, so a send blocks only on the kernel's page-granular accounting, and
// the write edges it raises are flushed by real turns beside the writers: the
// claim serializes the sends, and the records arrive whole. That needs TCP: a
// Unix socket's per-send charge fills Linux's default limit after 555 such
// records.
func TestDirectSendSharesTheWriteClaim(t *testing.T) {
	const records, recordSize = 4000, 8
	testConn := newTCPTestConnection(t, &Events{Pollers: 1})
	conn := testConn.conn
	if err := unix.SetsockoptInt(int(conn.fd), unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptInt(testConn.peer, unix.SOL_SOCKET, unix.SO_RCVBUF, 1<<20); err != nil {
		t.Fatal(err)
	}
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
		// Bytes the owner queued behind a write turn leave with the kick a
		// turn's release makes; flushOnLoop itself runs only inside a turn.
		conn.kickWriter()
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

// A read round's replies go to the socket straight from its reply space, and
// a full socket may take only part of them. A write another goroutine queues
// while that send runs must follow the replies' unsent rest rather than cut
// into it, or the peer reads a torn record.
func TestPartialReplySendKeepsLaterWritesBehind(t *testing.T) {
	const replies, recordSize = 16, 1000
	events := &Events{Pollers: 1, MaxBufferSize: 16 << 10}
	events.OnData = func(conn Conn) error {
		_, _ = conn.Discard(conn.InboundBuffered())
		for seq := range uint32(replies) {
			dst, err := conn.ReserveOutbound(recordSize)
			if err != nil {
				return err
			}
			fillTestRecord(dst, 'R', seq)
		}
		return nil
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The send buffer holds a fraction of the round's replies and the peer
	// reads nothing yet, so the round's send is partial.
	if err = unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 4<<10); err != nil {
		t.Fatal(err)
	}
	var injected atomic.Bool
	testHookScratchSend = func(conn *fdConn) {
		if !injected.CompareAndSwap(false, true) {
			return
		}
		// Another goroutine writes as the round starts its send: it finds the
		// write claim taken and leaves its bytes queued.
		done := make(chan error, 1)
		go func() {
			var record [recordSize]byte
			fillTestRecord(record[:], 'W', 0)
			_, err := conn.Write(record[:])
			done <- err
		}()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookScratchSend = nil })
	testConn, registered := startTestConnectionOn(t, events, fds)
	if err = <-registered; err != nil {
		t.Fatal(err)
	}

	if err = writePeerAll(testConn.peer, []byte{1}, time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	data := readPeerUntil(t, testConn.peer, (replies+1)*recordSize, 10*time.Second)
	if !injected.Load() {
		t.Fatal("the round never sent from its reply space")
	}
	for offset, seq := 0, uint32(0); offset < len(data); offset += recordSize {
		record := data[offset : offset+recordSize]
		if !validTestRecord(record) {
			t.Fatalf("record at byte %d is torn: tag %q", offset, record[0])
		}
		if record[0] == 'R' {
			if got := binary.LittleEndian.Uint32(record[1:]); got != seq {
				t.Fatalf("reply %d arrived as reply %d", got, seq)
			}
			seq++
		} else if offset != replies*recordSize {
			t.Fatalf("the other goroutine's write landed at byte %d, inside the round's replies", offset)
		}
	}
}

// writeFromOtherGoroutine writes data to conn from a goroutine of its own and
// waits for the write to return, so it is queued before the caller goes on.
func writeFromOtherGoroutine(t *testing.T, conn Conn, data []byte) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := conn.Write(data)
		done <- err
	}()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

// Bytes a round reserves are part of outbound when ReserveOutbound returns, so
// a write another goroutine makes afterwards follows them on the wire, as does
// everything the round writes or reserves after that write.
func TestReservedRepliesPrecedeLaterWrites(t *testing.T) {
	events := &Events{Pollers: 1}
	events.OnData = func(conn Conn) error {
		_, _ = conn.Discard(conn.InboundBuffered())
		reserve := func(tag byte) error {
			dst, err := conn.ReserveOutbound(1)
			if err != nil {
				return err
			}
			dst[0] = tag
			return nil
		}
		if err := reserve('a'); err != nil {
			return err
		}
		writeFromOtherGoroutine(t, conn, []byte{'B'})
		// The round's own write hands its reply space over mid-round.
		if _, err := conn.Write([]byte{'c'}); err != nil {
			return err
		}
		if err := reserve('d'); err != nil {
			return err
		}
		writeFromOtherGoroutine(t, conn, []byte{'E'})
		return reserve('f')
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	testConn, registered := startTestConnectionOn(t, events, fds)
	if err = <-registered; err != nil {
		t.Fatal(err)
	}
	if err = writePeerAll(testConn.peer, []byte{1}, time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := readPeerUntil(t, testConn.peer, 6, 5*time.Second); string(got) != "aBcdEf" {
		t.Fatalf("wire order %q, want %q", got, "aBcdEf")
	}
}

// With output already queued when the round reserves, the reservation goes
// behind it, and a later write from another goroutine still follows the
// reservation.
func TestReservedRepliesBehindQueuedOutput(t *testing.T) {
	const backlog = 256 << 10
	events := &Events{Pollers: 1}
	events.OnData = func(conn Conn) error {
		_, _ = conn.Discard(conn.InboundBuffered())
		// The peer reads nothing yet, so most of this stays queued.
		writeFromOtherGoroutine(t, conn, bytes.Repeat([]byte{'Q'}, backlog))
		dst, err := conn.ReserveOutbound(1)
		if errors.Is(err, ErrReserveUnsupported) {
			// A write turn holds the claim; the round writes instead.
			_, err = conn.Write([]byte{'r'})
		} else if err == nil {
			dst[0] = 'r'
		}
		if err != nil {
			return err
		}
		writeFromOtherGoroutine(t, conn, []byte{'W'})
		return nil
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 4<<10); err != nil {
		t.Fatal(err)
	}
	testConn, registered := startTestConnectionOn(t, events, fds)
	if err = <-registered; err != nil {
		t.Fatal(err)
	}
	if err = writePeerAll(testConn.peer, []byte{1}, time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	got := readPeerUntil(t, testConn.peer, backlog+2, 10*time.Second)
	if i := bytes.IndexFunc(got[:backlog], func(r rune) bool { return r != 'Q' }); i >= 0 {
		t.Fatalf("queued output interrupted at byte %d by %q", i, got[i])
	}
	if tail := string(got[backlog:]); tail != "rW" {
		t.Fatalf("after the queued output: %q, want %q", tail, "rW")
	}
}

// fillSocketUntilRefused writes to a non-blocking fd until it refuses even
// one byte.
func fillSocketUntilRefused(t *testing.T, fd int) int {
	t.Helper()
	total := 0
	chunk := make([]byte, 64<<10)
	for size := len(chunk); size > 0; size /= 2 {
		for {
			n, err := unix.Write(fd, chunk[:size])
			if n > 0 {
				total += n
			}
			if err != nil {
				if errors.Is(err, unix.EAGAIN) {
					break
				}
				t.Fatal(err)
			}
		}
	}
	return total
}

// drainPeerFully reads everything the peer's socket holds and reports how much.
func drainPeerFully(fd int) int {
	buf := make([]byte, 1<<20)
	total := 0
	for {
		n, err := unix.Read(fd, buf)
		if n > 0 {
			total += n
		}
		if err != nil || n <= 0 {
			return total
		}
	}
}

// A direct send a full socket refuses publishes its bytes and writeBlocked
// only after the send returned. The writable edge the drain raises must not be
// dropped in that window: it is the only thing that would ever move them.
func TestRefusedDirectSendKeepsAWritableEdge(t *testing.T) {
	payload := bytes.Repeat([]byte{'P'}, 100)
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 8<<10); err != nil {
		t.Fatal(err)
	}
	if err = unix.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	fillSocketUntilRefused(t, fds[0])
	var hooked atomic.Bool
	testHookDirectSendRefused = func(conn *fdConn) {
		if !hooked.CompareAndSwap(false, true) {
			return
		}
		// The peer frees the socket right after the refused send, and the
		// loop collects the writable edge before the turn publishes anything.
		drainPeerFully(fds[1])
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() { testHookDirectSendRefused = nil })
	events := &Events{Pollers: 1}
	events.OnOpen = func(conn Conn) {
		if _, err := conn.Write(payload); err != nil {
			t.Error(err)
		}
	}
	testConn, registered := startTestConnectionOn(t, events, fds)
	if err = <-registered; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !hooked.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hooked.Load() {
		t.Fatal("the direct send was never refused")
	}
	got := 0
	buf := make([]byte, 4096)
	for got < len(payload) && time.Now().Before(deadline) {
		n, _ := unix.Read(testConn.peer, buf)
		if n > 0 {
			got += n
		}
		time.Sleep(time.Millisecond)
	}
	if got < len(payload) {
		c := testConn.conn
		t.Fatalf("stalled: peer got %d of %d bytes; pending=%d writeBlocked=%v", got, len(payload), c.pending.Load(), c.writeBlocked())
	}
}

// A reply space that other goroutines have written behind continues only
// while it still holds every accepted byte: a reservation made after those
// writes belongs behind them, not inside the space.
func TestReserveAfterExternalWriteKeepsOrder(t *testing.T) {
	events := &Events{Pollers: 1}
	events.OnData = func(conn Conn) error {
		_, _ = conn.Discard(conn.InboundBuffered())
		reserve := func(tag byte) error {
			dst, err := conn.ReserveOutbound(1)
			if err != nil {
				return err
			}
			dst[0] = tag
			return nil
		}
		if err := reserve('a'); err != nil {
			return err
		}
		writeFromOtherGoroutine(t, conn, []byte{'B'})
		return reserve('c')
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	testConn, registered := startTestConnectionOn(t, events, fds)
	if err = <-registered; err != nil {
		t.Fatal(err)
	}
	if err = writePeerAll(testConn.peer, []byte{1}, time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := readPeerUntil(t, testConn.peer, 3, 5*time.Second); string(got) != "aBc" {
		t.Fatalf("wire order %q, want %q", got, "aBc")
	}
}

// fillTestRecord fills record with tag, seq and a trailer that ties them, so a
// record cut by other bytes fails validTestRecord.
func fillTestRecord(record []byte, tag byte, seq uint32) {
	for i := range record {
		record[i] = tag
	}
	binary.LittleEndian.PutUint32(record[1:], seq)
	record[len(record)-1] = tag ^ byte(seq)
}

func validTestRecord(record []byte) bool {
	tag := record[0]
	if tag != 'R' && tag != 'W' {
		return false
	}
	for _, b := range record[5 : len(record)-1] {
		if b != tag {
			return false
		}
	}
	return record[len(record)-1] == tag^byte(binary.LittleEndian.Uint32(record[1:]))
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

// An echo through the outbound limit over loopback TCP, where the socket rarely
// refuses a send, completes end to end. The restart of paused reads without a
// writable edge is pinned by the write-budget tests: a socket that turns
// writable while the backlog stays above the limit only resumes sending.
func TestWriteTurnResumesReadsUnderOutboundLimit(t *testing.T) {
	const total = 8 << 20
	replies := make(chan []byte, 4096)
	started := make(chan string, 1)
	opened := make(chan Conn, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: 8 << 10}
	events.OnStart = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
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

// A writable edge reaches the connection's turn while another sender may sit
// between its EAGAIN and recording it; that edge will not come again. The
// sender then retries instead of blocking, and an edge after the record
// clears it, so the holder's release sends again.
func TestWritableEdgeReachesABusySender(t *testing.T) {
	var conn fdConn
	conn.tryClaimWrite(0)
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
	// An edge handled before the claim was taken says nothing about the new
	// holder's sends: its EAGAIN blocks at once instead of retrying.
	conn.releaseWrite()
	conn.tryClaimWrite(0)
	if conn.markWriteBlocked() || !conn.writeBlocked() {
		t.Fatal("an edge from before the claim made its holder retry")
	}
}

// recoveringExecutor runs every task on a goroutine of its own and recovers
// panics, as an application executor that keeps its server alive does.
type recoveringExecutor struct{ recovered atomic.Int64 }

func (e *recoveringExecutor) Submit(task IOTask) bool {
	go func() {
		defer func() {
			if recover() != nil {
				e.recovered.Add(1)
			}
		}()
		task.RunTask()
	}()
	return true
}

func (e *recoveringExecutor) SubmitBatch(tasks []IOTask) int {
	for _, task := range tasks {
		e.Submit(task)
	}
	return len(tasks)
}

// An OnOutbound that panics during a flush, on a write turn or on the
// connection's own turn, gives the write claim back: later output is still
// sent, and Close and Serve return.
func TestPanicInOnOutboundReleasesTheWriteClaim(t *testing.T) {
	for _, fromWriteTurn := range []bool{true, false} {
		name := "connection turn"
		if fromWriteTurn {
			name = "write turn"
		}
		t.Run(name, func(t *testing.T) {
			executor := &recoveringExecutor{}
			opened := make(chan *fdConn, 1)
			started := make(chan string, 1)
			var armed atomic.Bool
			events := &Events{Pollers: 1, Executor: executor}
			events.OnStart = func(ev *Events) {
				for _, listener := range ev.acceptor.listeners {
					started <- listener.pair.local.String()
					return
				}
			}
			events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn) }
			events.OnData = func(conn Conn) error {
				_, _ = conn.Discard(-1)
				_, err := conn.Write([]byte("r"))
				return err
			}
			events.OnOutbound = func(Conn, int) {
				if armed.CompareAndSwap(true, false) {
					panic("OnOutbound panics once")
				}
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
			client, err := net.Dial("tcp", <-started)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server := <-opened
			for deadline := time.Now().Add(2 * time.Second); server.writeState.Load()&writeOpenedFlag == 0 ||
				server.ioOwner.Load() != 0; {
				if time.Now().After(deadline) {
					t.Fatal("connection did not finish opening")
				}
				time.Sleep(time.Millisecond)
			}
			armed.Store(true)
			first := "x"
			if fromWriteTurn {
				_, err = server.Write([]byte(first))
			} else {
				first = "r"
				_, err = client.Write([]byte("q"))
			}
			if err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(2 * time.Second); executor.recovered.Load() == 0; {
				if time.Now().After(deadline) {
					t.Fatal("OnOutbound did not panic")
				}
				time.Sleep(time.Millisecond)
			}
			if _, err = server.Write([]byte("later")); err != nil {
				t.Fatal(err)
			}
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			want := first + "later"
			got := make([]byte, len(want))
			if _, err = io.ReadFull(client, got); err != nil || string(got) != want {
				t.Fatalf("client read %q, %v; want %q", got, err, want)
			}
			_ = events.Close(nil)
			select {
			case <-serveDone:
			case <-time.After(3 * time.Second):
				t.Fatalf("Serve did not return after Close: writeState=%#x", server.writeState.Load())
			}
		})
	}
}

// A flush the socket refused part of, on a write turn or on the connection's
// own turn, still arms writable interest when the OnOutbound it then runs
// panics: the rest of the output is sent once the peer reads.
func TestPanicAfterBlockedFlushKeepsSending(t *testing.T) {
	for _, fromWriteTurn := range []bool{true, false} {
		name := "connection turn"
		if fromWriteTurn {
			name = "write turn"
		}
		t.Run(name, func(t *testing.T) {
			const total = 2 << 20
			payload := make([]byte, total)
			for i := range payload {
				payload[i] = byte(i * 11)
			}
			executor := &recoveringExecutor{}
			opened := make(chan *fdConn, 1)
			started := make(chan string, 1)
			var armed atomic.Bool
			var blockedAtPanic, ownerAtPanic atomic.Bool
			events := &Events{Pollers: 1, Executor: executor}
			events.OnStart = func(ev *Events) {
				for _, listener := range ev.acceptor.listeners {
					started <- listener.pair.local.String()
					return
				}
			}
			events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn) }
			events.OnData = func(conn Conn) error {
				_, _ = conn.Discard(-1)
				_, err := conn.Write(payload)
				return err
			}
			events.OnOutbound = func(c Conn, _ int) {
				if armed.CompareAndSwap(true, false) {
					conn := c.(*fdConn)
					blockedAtPanic.Store(conn.writeBlocked())
					ownerAtPanic.Store(conn.directOwner())
					panic("OnOutbound panics once")
				}
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
			t.Cleanup(func() {
				_ = events.Close(nil)
				<-serveDone
			})
			// Small buffers on both ends, and a peer that does not read yet, so
			// the first flush is refused before it reports what it sent. The
			// peer's buffer is set before it connects: shrinking it below a
			// window it already advertised makes Linux drop segments, and the
			// retransmission backoff would stall this test on its own.
			dialer := net.Dialer{Control: func(_, _ string, raw syscall.RawConn) error {
				var sockErr error
				if err := raw.Control(func(fd uintptr) {
					sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
				}); err != nil {
					return err
				}
				return sockErr
			}}
			client, err := dialer.Dial("tcp", <-started)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server := <-opened
			for deadline := time.Now().Add(2 * time.Second); server.writeState.Load()&writeOpenedFlag == 0 ||
				server.ioOwner.Load() != 0; {
				if time.Now().After(deadline) {
					t.Fatal("connection did not finish opening")
				}
				time.Sleep(time.Millisecond)
			}
			if err := unix.SetsockoptInt(int(server.fd), unix.SOL_SOCKET, unix.SO_SNDBUF, 4096); err != nil {
				t.Fatal(err)
			}
			armed.Store(true)
			if fromWriteTurn {
				_, err = server.Write(payload)
			} else {
				_, err = client.Write([]byte("q"))
			}
			if err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(2 * time.Second); executor.recovered.Load() == 0; {
				if time.Now().After(deadline) {
					t.Fatal("OnOutbound did not panic")
				}
				time.Sleep(time.Millisecond)
			}
			if !blockedAtPanic.Load() {
				t.Fatal("the flush that ran OnOutbound was not refused by the socket")
			}
			if ownerAtPanic.Load() == fromWriteTurn {
				t.Fatalf("OnOutbound ran on the wrong turn: connection turn = %v", ownerAtPanic.Load())
			}
			_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
			got := make([]byte, total)
			if n, err := io.ReadFull(client, got); err != nil {
				t.Fatalf("peer read %d of %d bytes: %v (pending=%d writeState=%#x)",
					n, total, err, server.pending.Load(), server.writeState.Load())
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("peer received a different stream")
			}
		})
	}
}

// A read round that yielded is redelivered even when OnOutbound panics in the
// turn's final flush: bytes already in the socket bring no new edge.
func TestPanicAfterYieldedReadKeepsReading(t *testing.T) {
	executor := &recoveringExecutor{}
	opened := make(chan *fdConn, 1)
	started := make(chan string, 1)
	var armed, stalledAtPanic atomic.Bool
	events := &Events{Pollers: 1, Executor: executor, MaxBufferSize: 1}
	events.OnStart = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn) }
	events.OnData = func(conn Conn) error {
		buf := make([]byte, conn.InboundBuffered())
		_, _ = io.ReadFull(conn, buf)
		if _, err := conn.Write(buf); err != nil {
			return err
		}
		return conn.YieldRead()
	}
	events.OnOutbound = func(c Conn, _ int) {
		if armed.CompareAndSwap(true, false) {
			stalledAtPanic.Store(c.(*fdConn).readStalled())
			panic("OnOutbound panics once")
		}
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
	for deadline := time.Now().Add(2 * time.Second); server.writeState.Load()&writeOpenedFlag == 0 ||
		server.ioOwner.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("connection did not finish opening")
		}
		time.Sleep(time.Millisecond)
	}
	armed.Store(true)
	// One read takes one byte; the round yields after it, leaving the second
	// byte in the socket.
	if _, err := client.Write([]byte("qw")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 2)
	if n, err := io.ReadFull(client, got); err != nil || string(got) != "qw" {
		t.Fatalf("client read %q (%d bytes), %v; want %q (readStalled=%v)", got[:n], n, err, "qw", server.readStalled())
	}
	// The turn's epilogue resumes reading while the panic unwinds, so the
	// echo can arrive before the executor has counted the recovery.
	for deadline := time.Now().Add(2 * time.Second); executor.recovered.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("OnOutbound did not panic after a yielded read")
		}
		time.Sleep(time.Millisecond)
	}
	if !stalledAtPanic.Load() {
		t.Fatal("OnOutbound panicked before the yielded read had stalled")
	}
}

// OnOutbound runs inside the flush that reports the bytes. A Write it makes is
// queued and sent by a later flush, and a Flush it calls returns at once,
// never sending from the same call stack, which would call OnOutbound again inside itself: a callback that
// writes more whenever output drains must not nest without bound or deadlock
// on its own lock. This holds whether the connection's turn took the claim
// for the flush or keeps it for a reservation.
func TestOnOutboundWritesDoNotReenterIt(t *testing.T) {
	for _, reserve := range []bool{false, true} {
		name := "Write"
		if reserve {
			name = "ReserveOutbound"
		}
		t.Run(name, func(t *testing.T) {
			const total = 20000
			var depth, maxDepth, count atomic.Int32
			var mu sync.Mutex
			events := &Events{Pollers: 1}
			events.OnData = func(c Conn) error {
				_, _ = c.Discard(-1)
				if reserve {
					dst, err := c.ReserveOutbound(1)
					if err == nil {
						dst[0] = 'a'
						return nil
					}
				}
				_, err := c.Write([]byte("a"))
				return err
			}
			events.OnOutbound = func(c Conn, n int) {
				mu.Lock() // deadlocks if the callback re-enters itself
				defer mu.Unlock()
				if d := depth.Add(1); d > maxDepth.Load() {
					maxDepth.Store(d)
				}
				defer depth.Add(-1)
				if count.Add(1) < total {
					_, _ = c.Write([]byte("b"))
					_ = c.Flush()
				}
			}
			testConn := newTestConnection(t, events)
			if _, err := unix.Write(testConn.peer, []byte("q")); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(10 * time.Second); count.Load() < total; {
				if time.Now().After(deadline) {
					t.Fatalf("OnOutbound ran %d of %d times", count.Load(), total)
				}
				buf := make([]byte, 64<<10)
				if _, err := unix.Read(testConn.peer, buf); err != nil {
					// The peer is non-blocking: spinning on it would hold the
					// only P with GOMAXPROCS=1 until the next preemption.
					runtime.Gosched()
				}
			}
			if d := maxDepth.Load(); d != 1 {
				t.Fatalf("OnOutbound nested %d deep", d)
			}
		})
	}
}

// A Write made while the connection's turn keeps the claim for a reservation
// it already flushed is queued for the turn's flush, never sent directly: sent
// directly, OnOutbound would run inside that send and a Write it makes would
// call it again.
func TestOnOutboundDoesNotReenterAfterAFlushedReservation(t *testing.T) {
	var armed atomic.Bool
	var depth, maxDepth, count atomic.Int32
	opened := make(chan struct{})
	events := &Events{Pollers: 1}
	events.OnOpen = func(c Conn) {
		defer close(opened)
		dst, err := c.ReserveOutbound(1)
		if err != nil {
			t.Errorf("ReserveOutbound: %v", err)
			return
		}
		dst[0] = 'r'
		if err = c.Flush(); err != nil {
			t.Error(err)
		}
		armed.Store(true)
		if _, err = c.Write([]byte("o")); err != nil {
			t.Error(err)
		}
	}
	events.OnOutbound = func(c Conn, n int) {
		if !armed.Load() {
			return
		}
		d := depth.Add(1)
		defer depth.Add(-1)
		if d > maxDepth.Load() {
			maxDepth.Store(d)
		}
		if count.Add(1) < 6 {
			_, _ = c.Write([]byte("b"))
		}
	}
	testConn := newTestConnection(t, events)
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("OnOpen did not run")
	}
	if got := string(readPeerUntil(t, testConn.peer, 7, 2*time.Second)); got != "robbbbb" {
		t.Fatalf("peer received %q", got)
	}
	if d := maxDepth.Load(); d > 1 {
		t.Fatalf("OnOutbound nested %d deep", d)
	}
}

// OnOpen that panics under an Executor that recovers it still lets output from
// other goroutines go out: what they queued while OnOpen ran leaves without
// waiting for another write, and later output starts write turns.
func TestOutputAfterRecoveredOnOpenPanic(t *testing.T) {
	executor := &recoveringExecutor{}
	opened := make(chan Conn, 1)
	started := make(chan string, 1)
	events := &Events{Pollers: 1, Executor: executor}
	events.OnStart = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	events.OnOpen = func(conn Conn) {
		written := make(chan error, 1)
		go func() {
			_, err := conn.Write([]byte("early"))
			written <- err
		}()
		if err := <-written; err != nil {
			t.Error(err)
		}
		opened <- conn
		panic("OnOpen panics")
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
	for deadline := time.Now().Add(2 * time.Second); executor.recovered.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("OnOpen did not panic")
		}
		time.Sleep(time.Millisecond)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 5)
	if _, err = io.ReadFull(client, buf); err != nil || string(buf) != "early" {
		t.Fatalf("client read %q, %v: output queued during OnOpen stayed queued", buf, err)
	}
	if _, err = server.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadFull(client, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("client read %q, %v after a recovered OnOpen panic", buf, err)
	}
}

// A write turn that finds nothing left releases its claim while another
// goroutine may be queuing output: that producer found the claim taken and
// left its bytes to the turn, so the release looks again and sends them.
func TestWriteTurnReleaseSendsOutputQueuedBehindIt(t *testing.T) {
	testConn := newTestConnection(t, &Events{Pollers: 1})
	conn := testConn.conn
	for deadline := time.Now().Add(2 * time.Second); conn.writeState.Load()&writeOpenedFlag == 0 ||
		conn.ioOwner.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("connection did not finish opening")
		}
		time.Sleep(time.Millisecond)
	}
	// A write turn holds the claim and its turn count...
	if !conn.loop.acquireIO() || !conn.tryClaimWrite(0) {
		t.Fatal("could not stand in for a write turn")
	}
	// ...when another goroutine queues output, whose kick finds it taken.
	written := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("late"))
		written <- err
	}()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	conn.finishWriteTurn()
	if got := string(readPeerUntil(t, testConn.peer, 4, 2*time.Second)); got != "late" {
		t.Fatalf("peer received %q", got)
	}
}

// A kick that takes the claim after shutdown began gives it straight back:
// closing a connection waits for the claim, so keeping it would hang Serve.
func TestKickAfterShutdownBeganReleasesTheClaim(t *testing.T) {
	events := &Events{Pollers: 1}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		loop.ioPool.stop()
		_ = loop.poller.Close(nil)
	}()
	conn := &fdConn{fd: -1}
	conn.events, conn.loop = events, loop
	conn.writeState.Store(writeOpenedFlag)
	conn.pending.Store(1)
	loop.stopIO()
	conn.kickWriter()
	if conn.writeClaimed() {
		t.Fatal("a kick after shutdown began kept the write claim")
	}
}

// A direct send that the socket takes in part keeps its suffix ahead of bytes
// another goroutine queued during the send: OnOpen's large Write is not split.
func TestDirectSendSuffixStaysWhole(t *testing.T) {
	for attempt := 0; attempt < 10; attempt++ {
		direct := bytes.Repeat([]byte{'D'}, 4<<20)
		producer := bytes.Repeat([]byte{'P'}, 64)
		opened := make(chan struct{}, 1)
		producerDone := make(chan error, 1)
		events := &Events{Pollers: 1}
		events.OnOpen = func(c Conn) {
			fc := c.(*fdConn)
			go func() {
				// The direct sender takes the claim just before its send.
				for deadline := time.Now().Add(time.Second); fc.writeState.Load()&writeOwnerFlag == 0 &&
					time.Now().Before(deadline); {
				}
				_, err := fc.Write(producer)
				producerDone <- err
			}()
			if _, err := c.Write(direct); err != nil {
				t.Error(err)
			}
			opened <- struct{}{}
		}
		testConn := newTestConnection(t, events)
		<-opened
		if err := <-producerDone; err != nil {
			t.Fatal(err)
		}
		got := readPeerUntil(t, testConn.peer, len(direct)+len(producer), 10*time.Second)
		if first := bytes.IndexByte(got, 'P'); first != 0 && first != len(direct) {
			t.Fatalf("attempt %d: OnOpen payload split by producer bytes at offset %d", attempt, first)
		}
		testConn.stop()
	}
}

// Contention means a goroutine other than the one a write turn's claim was
// taken for queued output: a lone producer that writes several times per
// request does not make its own write turn yield.
func TestOnlyOtherProducersMarkContention(t *testing.T) {
	testConn := newTestConnection(t, &Events{Pollers: 1})
	conn := testConn.conn
	for deadline := time.Now().Add(2 * time.Second); conn.writeState.Load()&writeOpenedFlag == 0 ||
		conn.ioOwner.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("connection did not finish opening")
		}
		time.Sleep(time.Millisecond)
	}
	conn.pending.Add(1)
	defer conn.pending.Add(-1)
	if !conn.tryClaimWrite(claimerTag()) {
		t.Fatal("could not take the claim")
	}
	defer conn.releaseWrite()
	conn.kickWriter()
	if conn.writeState.Load()&writeContendedFlag != 0 {
		t.Fatal("the claim's own producer marked contention")
	}
	done := make(chan struct{})
	go func() {
		conn.kickWriter()
		close(done)
	}()
	<-done
	if conn.writeState.Load()&writeContendedFlag == 0 {
		t.Fatal("another goroutine's output did not mark contention")
	}
}

// writeTurnRejectingExecutor runs connection turns and refuses write turns.
type writeTurnRejectingExecutor struct{ refused atomic.Int64 }

func (e *writeTurnRejectingExecutor) Submit(task IOTask) bool {
	if _, ok := task.(*writeTurn); ok {
		e.refused.Add(1)
		return false
	}
	go task.RunTask()
	return true
}

func (e *writeTurnRejectingExecutor) SubmitBatch(tasks []IOTask) int {
	for index, task := range tasks {
		if !e.Submit(task) {
			return index
		}
	}
	return len(tasks)
}

// A write turn the Executor refuses closes its connection, as a refused
// connection turn does: the queued bytes have no other sender.
func TestRefusedWriteTurnClosesTheConnection(t *testing.T) {
	executor := &writeTurnRejectingExecutor{}
	opened := make(chan *fdConn, 1)
	closed := make(chan error, 1)
	started := make(chan string, 1)
	events := &Events{Pollers: 1, Executor: executor}
	events.OnStart = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn) }
	events.OnClose = func(_ Conn, err error) { closed <- err }
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
	for deadline := time.Now().Add(2 * time.Second); server.writeState.Load()&writeOpenedFlag == 0 ||
		server.ioOwner.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("connection did not finish opening")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = server.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-closed:
		if !errors.Is(err, net.ErrClosed) || executor.refused.Load() == 0 {
			t.Fatalf("OnClose(%v) after %d refused write turns", err, executor.refused.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the refused write turn did not close the connection")
	}
}

// A reservation the outbound limit refuses leaves the claim free, so output
// from other goroutines is not held back until the turn ends.
func TestRefusedReservationReleasesTheWriteClaim(t *testing.T) {
	result := make(chan error, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: 1024}
	events.OnData = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		_, err := conn.ReserveOutbound(4096)
		if fdc := conn.(*fdConn); err == nil || fdc.writeClaimed() {
			err = fmt.Errorf("ReserveOutbound = %v with the claim taken: %v", err, fdc.writeClaimed())
		} else {
			err = nil
		}
		result <- err
		return nil
	}
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not run")
	}
}

// Closing while write turns are sending must never write a closed descriptor:
// its number is reused at once by the next socket of this process, which may
// be either end of the next connection. Neither client ever sends, so any byte
// the server receives, or connection B's client reads, came from A's senders.
// The window is a few instructions wide, so this is a smoke test; close taking
// the write claim before the descriptor goes is what rules it out.
func TestCloseNeverWritesReusedDescriptor(t *testing.T) {
	opened := make(chan *fdConn, 4)
	closed := make(chan *fdConn, 4)
	started := make(chan string, 1)
	var strayInbound atomic.Int64
	events := &Events{Pollers: 1}
	events.OnStart = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
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
			started <- listener.pair.local.String()
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

// Writev takes vectors longer than one writev or sendmsg allows, which is
// 1024 buffers on Linux and the BSDs, whether it sends them at once, as from
// OnOpen, or queues them behind a read round's other replies.
func TestWritevTakesVectorsLongerThanOneSyscall(t *testing.T) {
	for _, callback := range []string{"OnOpen", "OnData"} {
		t.Run(callback, func(t *testing.T) {
			vec := make([][]byte, 2000)
			var want []byte
			for index := range vec {
				vec[index] = []byte{byte('a' + index%26)}
				want = append(want, vec[index]...)
			}
			events := &Events{Pollers: 1}
			started := make(chan string, 1)
			result := make(chan error, 1)
			writev := func(conn Conn) {
				n, err := conn.Writev(vec)
				if err == nil && n != len(want) {
					err = io.ErrShortWrite
				}
				result <- err
			}
			events.OnStart = func(ev *Events) {
				for _, listener := range ev.acceptor.listeners {
					started <- listener.pair.local.String()
					return
				}
			}
			events.OnOpen = func(conn Conn) {
				if callback == "OnOpen" {
					writev(conn)
				}
			}
			events.OnData = func(conn Conn) error {
				_, _ = conn.Discard(conn.InboundBuffered())
				if callback == "OnData" {
					writev(conn)
				}
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
			if _, err = client.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if err = <-result; err != nil {
				t.Fatalf("Writev of %d buffers in %s: %v", len(vec), callback, err)
			}
			if err = client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if _, err = io.ReadFull(client, got); err != nil {
				t.Fatalf("peer read: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("peer received different bytes")
			}
		})
	}
}

// A read round's first owned output leaves for the socket while the callback
// that wrote it is still running — nothing of it shows as buffered — and the
// writes after it coalesce into the round's final flush. This is the shape of
// a reply batch handed over as one owned buffer: the round it was assembled
// in is usually its only writer. A write the threshold would buffer keeps
// joining the round's batch in the first position, so the configuration's
// promise holds there as everywhere.
func TestCorkedRoundWritesLeaveTogetherAtRoundEnd(t *testing.T) {
	// A corked round never sends directly: both writes stay queued, and the
	// round's final flush hands the peer one "onetwo" in order. The buffered
	// threshold plays no part inside the round.
	tests := []struct {
		name      string
		threshold int
	}{
		{name: "threshold-disabled", threshold: 0},
		{name: "below-threshold-joins-the-batch", threshold: 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type buffered struct{ first, second int }
			events := &Events{Pollers: 1, WriteBufferedThreshold: tt.threshold}
			started := make(chan string, 1)
			measured := make(chan buffered, 8)
			events.OnStart = func(ev *Events) {
				for _, listener := range ev.acceptor.listeners {
					started <- listener.pair.local.String()
					return
				}
			}
			ownedWrite := func(conn Conn, value string) error {
				owned := AcquireBuffer(len(value))
				copy(owned.AvailableBuffer()[:len(value)], value)
				owned.CommitWrite(len(value))
				_, err := conn.WriteOwned(owned)
				return err
			}
			events.OnData = func(conn Conn) error {
				_, _ = conn.Discard(conn.InboundBuffered())
				if err := ownedWrite(conn, "one"); err != nil {
					return err
				}
				first := conn.OutboundBuffered()
				if err := ownedWrite(conn, "two"); err != nil {
					return err
				}
				measured <- buffered{first: first, second: conn.OutboundBuffered()}
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
			if _, err = client.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			var got buffered
			select {
			case got = <-measured:
			case <-time.After(3 * time.Second):
				t.Fatal("the round never ran")
			}
			if got.first != 3 {
				t.Fatalf("buffered after the round's first write = %d, want 3", got.first)
			}
			if got.second != 6 {
				t.Fatalf("buffered after the round's second write = %d, want 6", got.second)
			}
			// The round's final flush publishes both writes, in order.
			if err = client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			wire := make([]byte, 6)
			if _, err = io.ReadFull(client, wire); err != nil {
				t.Fatalf("peer read: %v", err)
			}
			if string(wire) != "onetwo" {
				t.Fatalf("peer received %q, want %q", wire, "onetwo")
			}
		})
	}
}

// TestCoalesceOwnedCopiesOnlySmallOrSparseBuffers pins which owned writes a
// corked round copies into its outbound blocks: small ones and mostly empty
// ones, never one past half a coalescing block, and never a large, dense one
// such as a write batch carrying a whole read's replies.
func TestCoalesceOwnedCopiesOnlySmallOrSparseBuffers(t *testing.T) {
	const block = 32 << 10
	cases := []struct {
		name      string
		size, cap int
		want      bool
	}{
		{"small", 512, 16 << 10, true},
		{"at the small limit", coalesceOwnedMax, coalesceOwnedMax, true},
		{"large and full", 16120, 16 << 10, false},
		{"large and half empty", 6 << 10, 16 << 10, true},
		{"large and half full", 8 << 10, 16 << 10, false},
		{"past half a block", block/2 + 1, 64 << 10, false},
	}
	for _, c := range cases {
		owned := bytebuf.AcquireBuffer(c.cap)
		owned.CommitWrite(c.size)
		if got := coalesceOwned(owned, c.size, block); got != c.want {
			t.Errorf("%s: coalesceOwned(%d of %d) = %v, want %v", c.name, c.size, owned.Cap(), got, c.want)
		}
		bytebuf.ReleaseBuffer(owned)
	}
}

// runBidirectionalLimitTransfer pins that outbound backlog never stops reads:
// both peers fill their own backlog past the limit's old pause mark while the
// sockets hold small buffers, exactly the shape that used to leave each side
// waiting for the other to read. Every byte must reach the far OnData.
func runBidirectionalLimitTransfer(t *testing.T, raw [2]net.Conn, limit int) {
	t.Helper()
	total := int64(limit) * 7 / 8 // past the old pause mark, below the limit
	releaseOpen := make(chan struct{})
	var received [2]atomic.Int64
	var conns [2]Conn
	var events [2]*Events
	var done [2]chan error
	for i := range 2 {
		started := make(chan struct{})
		opened := make(chan Conn, 1)
		ev := &Events{Pollers: 1, MaxOutboundBuffered: limit, MaxBufferSize: 16 << 10}
		ev.OnStart = func(*Events) { close(started) }
		ev.OnOpen = func(conn Conn) {
			opened <- conn
			// Hold the open until both sides have their output queued, so
			// neither reads while the other is still filling.
			<-releaseOpen
		}
		ev.OnData = func(conn Conn) error {
			n, _ := conn.Discard(conn.InboundBuffered())
			received[i].Add(int64(n))
			return nil
		}
		events[i] = ev
		done[i] = make(chan error, 1)
		go func() { done[i] <- ev.Serve() }()
		<-started
		conn, err := ev.Adopt(raw[i], nil)
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = conn
		<-opened
	}
	t.Cleanup(func() {
		for _, ev := range events {
			_ = ev.Close(nil)
		}
		for _, ch := range done {
			select {
			case <-ch:
			case <-time.After(3 * time.Second):
				t.Error("Serve did not stop")
			}
		}
	})

	payload := make([]byte, total)
	for _, conn := range conns {
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	close(releaseOpen)

	// The stall detector reports a mutual wait within a second; the deadline
	// only bounds a slow-but-moving transfer, which a pinned-window pair under
	// -race can be.
	last, lastMoved := int64(-1), time.Now()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		a, b := received[0].Load(), received[1].Load()
		if a == total && b == total {
			return
		}
		if a+b != last {
			last, lastMoved = a+b, time.Now()
		}
		if time.Since(lastMoved) > time.Second {
			first, second := conns[0].(*fdConn), conns[1].(*fdConn)
			t.Fatalf("both directions stopped: received %d/%d and %d/%d; pending %d,%d; readStalled %v,%v; writeBlocked %v,%v",
				a, total, b, total, first.pending.Load(), second.pending.Load(),
				first.readStalled(), second.readStalled(), first.writeBlocked(), second.writeBlocked())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bidirectional transfer did not complete")
}

func TestBidirectionalOutboundLimitUnix(t *testing.T) {
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	var raw [2]net.Conn
	for i, fd := range sockets {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, pinnedSocketBuffer)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, pinnedSocketBuffer)
		file := os.NewFile(uintptr(fd), "bidirectional-peer")
		raw[i], err = net.FileConn(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		defer raw[i].Close()
	}
	runBidirectionalLimitTransfer(t, raw, 1<<20)
}

func TestBidirectionalOutboundLimitTCP(t *testing.T) {
	first, second := tcpConnectionPair(t)
	defer first.Close()
	defer second.Close()
	for _, conn := range []net.Conn{first, second} {
		_ = conn.(*net.TCPConn).SetReadBuffer(pinnedSocketBuffer)
		_ = conn.(*net.TCPConn).SetWriteBuffer(pinnedSocketBuffer)
	}
	// A smaller budget than the Unix case keeps the pinned-window transfer
	// bounded under -race on a loaded host; the mechanism is the same: the
	// backlog sits past the old pause mark for the whole transfer.
	runBidirectionalLimitTransfer(t, [2]net.Conn{first, second}, 2<<20)
}

// A write from another goroutine that passed its entry check before a
// CloseWrite completed is still refused when it comes to queue its bytes:
// queueing them after the end of the stream would leave them unsendable.
func TestQueuedWriteAfterCloseWriteIsRefused(t *testing.T) {
	events := &Events{Pollers: 1}
	client, server := serveInboundTest(t, events)
	if err := server.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := readAllWithin(t, client, 2*time.Second); len(got) != 0 {
		t.Fatalf("read %q before the end of stream", got)
	}
	conn := server.(*fdConn)
	if _, err := conn.queueOwnedWrite(bytebuf.CloneBuffer([]byte("late")), 4); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("queueing after CloseWrite = %v, want net.ErrClosed", err)
	}
	if n := conn.OutboundBuffered(); n != 0 {
		t.Fatalf("%d bytes queued after the end of stream", n)
	}
}

// A CloseWrite waiting for output that a full socket holds back schedules no
// turns while it waits: the FIN follows the writable edge that lets the
// output drain, and the peer then reads everything before the end of stream.
func TestCloseWriteWaitsForAFullSocketWithoutSpinning(t *testing.T) {
	executor := &countingExecutor{}
	want := make([]byte, 8<<20)
	for i := range want {
		want[i] = byte(i * 13)
	}
	queued := make(chan error, 1)
	events := &Events{
		Pollers:  1,
		Executor: executor,
		OnOpen: func(c Conn) {
			_, err := c.Write(want)
			if err == nil {
				err = c.CloseWrite()
			}
			queued <- err
		},
	}
	client, _ := serveInboundTest(t, events)
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the peer reads nothing: the socket stays full
	before := executor.turns.Load()
	time.Sleep(100 * time.Millisecond)
	if spun := executor.turns.Load() - before; spun > 10 {
		t.Fatalf("%d turns scheduled in 100ms while CloseWrite waited for a full socket", spun)
	}
	if got := readAllWithin(t, client, 20*time.Second); !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes before the end of stream, want %d intact", len(got), len(want))
	}
}

// A half-open connection that answers its peer's end of stream with
// CloseWrite closes in the turn that read the end of stream, as a connection
// that does not allow half-open does: the CloseWrite costs no turn of its own.
func TestHalfOpenCloseTakesNoExtraTurn(t *testing.T) {
	for _, halfOpen := range []bool{false, true} {
		executor := &countingExecutor{}
		closed := make(chan struct{})
		events := &Events{Pollers: 1, Executor: executor, AllowHalfOpen: halfOpen}
		events.OnData = func(c Conn) error {
			if _, err := c.WriteTo(c); err != nil {
				return err
			}
			if _, err := c.Read(nil); err == io.EOF {
				return c.CloseWrite()
			}
			return nil
		}
		events.OnClose = func(Conn, error) { close(closed) }
		client, _ := serveInboundTest(t, events)
		if _, err := client.Write(make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(client, make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		before := executor.turns.Load()
		_ = client.Close()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatalf("half-open %v: no close after the peer's end of stream", halfOpen)
		}
		time.Sleep(20 * time.Millisecond)
		if turns := executor.turns.Load() - before; turns != 1 {
			t.Fatalf("half-open %v: closing took %d turns, want 1", halfOpen, turns)
		}
	}
}

func TestExternalUDPServerPeerWrites(t *testing.T) {
	started := make(chan string, 1)
	peer := make(chan Conn, 1)
	closed := make(chan struct{}, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: 8}
	events.OnStart = func(ev *Events) {
		ev.acceptor.mux.Lock()
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
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

// A UDP write from outside the connection's turn is sent on the caller's
// goroutine, holding the server's submitMu: a child whose server is released
// has no socket left to send on.
func TestUDPChildWriteAfterServerRelease(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	events := &Events{MaxBufferSize: 64}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = loop.poller.Close(nil)
		loop.ioPool.stop()
	}()
	server := &fdConn{fd: int32(fds[0]), udp: &unixUDPState{peers: map[socket.UDPAddress]*fdConn{}}}
	server.events, server.loop, server.internal = events, loop, true
	child := &fdConn{fd: int32(fds[0]), udp: &unixUDPState{server: server}}
	child.events, child.loop = events, loop
	if n, err := child.Write([]byte("x")); n != 1 || err != nil {
		t.Fatalf("child write = %d, %v", n, err)
	}
	if got := string(readPeer(t, fds[1], 1)); got != "x" {
		t.Fatalf("datagram = %q", got)
	}
	if _, ok := server.teardown(nil); !ok {
		t.Fatal("server was not released")
	}
	if n, err := child.Write([]byte("y")); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after the server's release = %d, %v", n, err)
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
	defer func() {
		_ = loop.poller.Close(nil)
		loop.ioPool.stop()
	}()
	conn := &fdConn{fd: int32(fds[0]), udp: &unixUDPState{}}
	conn.events, conn.loop = events, loop
	if n, err := conn.Write(payload); n != 0 || !isUDPSendBlocked(err) {
		t.Fatalf("blocked external UDP write = %d, %v", n, err)
	}
	if conn.isClosing() || conn.OutboundBuffered() != 0 {
		t.Fatalf("blocked UDP write closed or retained bytes: closing=%v pending=%d", conn.isClosing(), conn.OutboundBuffered())
	}
}

// An external UDP write is sent before it returns, so a Close after it never
// overtakes it, and a write after the connection is released fails.
func TestExternalUDPWriteAndCloseOrdering(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		name := "write_before_close"
		if closeFirst {
			name = "write_after_release"
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
			conn := &fdConn{fd: int32(fds[0]), udp: &unixUDPState{}}
			conn.events, conn.loop = events, loop
			defer func() {
				if !conn.isReleased() {
					_ = unix.Close(fds[0])
				}
			}()
			if closeFirst {
				if _, ok := conn.teardown(nil); !ok {
					t.Fatal("connection was not released")
				}
				if n, err := conn.Write([]byte("packet")); n != 0 || !errors.Is(err, net.ErrClosed) {
					t.Fatalf("write after release = %d, %v", n, err)
				}
				return
			}
			if n, err := conn.Write([]byte("packet")); n != len("packet") || err != nil {
				t.Fatalf("write before close = %d, %v", n, err)
			}
			if err := conn.CloseWith(nil); err != nil {
				t.Fatal(err)
			}
			if got := string(readPeer(t, fds[1], len("packet"))); got != "packet" {
				t.Fatalf("datagram before close = %q", got)
			}
			if got := conn.OutboundBuffered(); got != 0 {
				t.Fatalf("pending bytes after close = %d", got)
			}
		})
	}
}
