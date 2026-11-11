//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/urpc/uio/internal/socket"
)

func queuedSegment(owner uint64, remain, sending int64, trailing string) fileSegment {
	seg := fileSegment{fd: -1, remain: remain, owner: owner, sending: sending}
	_, _ = seg.trailing.Write([]byte(trailing))
	return seg
}

func trailingOf(seg *fileSegment) string {
	var b bytes.Buffer
	vec, _ := seg.trailing.PeekVec(nil)
	for _, chunk := range vec {
		b.Write(chunk)
	}
	return b.String()
}

// Withdrawing one owner's ranges keeps everyone else's, and the bytes written
// after a withdrawn range move to what precedes it, so the stream keeps its
// order. Of the range a sendfile(2) is reading, the part the call reads stays:
// it may be on the wire already.
func TestWithdrawingRangesKeepsOtherOwnersInOrder(t *testing.T) {
	var c fdConn
	zc := c.zeroCopyState()
	for _, seg := range []fileSegment{
		queuedSegment(1, 100, 30, "a"),
		queuedSegment(2, 100, 0, "b"),
		queuedSegment(1, 100, 0, "c"),
		queuedSegment(2, 100, 0, "d"),
	} {
		zc.files.pushBack(seg)
	}
	c.writeState.Or(writeFileTailFlag)
	c.pending.Store(404)

	if got := c.cancelQueuedFiles(1); got != 170 {
		t.Fatalf("withdrew %d bytes of owner 1, want 170: the 70 past the part being sent and the 100 of its other range", got)
	}
	head := zc.files.front()
	if zc.files.len() != 3 || head.remain != 30 || head.sending != 30 || head.owner != 0 {
		t.Fatalf("after withdrawing owner 1: %d ranges, head remain %d sending %d owner %d",
			zc.files.len(), head.remain, head.sending, head.owner)
	}
	if got := trailingOf(&zc.files.segs[zc.files.head+1]); got != "bc" {
		t.Fatalf("bytes after the withdrawn range = %q, want them after the range before it: %q", got, "bc")
	}
	if c.pending.Load() != 234 {
		t.Fatalf("pending = %d, want 234", c.pending.Load())
	}
	if got := c.cancelQueuedFiles(1); got != 0 {
		t.Fatalf("a second withdrawal of owner 1 took %d bytes of the part being sent", got)
	}

	if got := c.cancelQueuedFiles(2); got != 200 {
		t.Fatalf("withdrew %d bytes of owner 2, want 200", got)
	}
	if zc.files.len() != 1 || trailingOf(zc.files.front()) != "abcd" {
		t.Fatalf("after withdrawing owner 2: %d ranges, head trailing %q", zc.files.len(), trailingOf(zc.files.front()))
	}

	// Once nothing reads it, a withdrawn head's bytes go to outbound.
	zc.files.front().sending = 0
	zc.files.front().owner = 3
	if got := c.cancelQueuedFiles(3); got != 30 || zc.files.len() != 0 || c.outbound.Len() != 4 {
		t.Fatalf("withdrawal of an idle head = %d, %d ranges, %d bytes in outbound", got, zc.files.len(), c.outbound.Len())
	}
	if c.writeState.Load()&writeFileTailFlag != 0 {
		t.Fatal("writeFileTailFlag outlived the last queued range")
	}
}

// The queue pops its front without moving the rest, reuses that room before
// it grows, and takes a range at its front.
func TestFileQueueKeepsOrderAcrossPopsAndPushes(t *testing.T) {
	var q fileQueue
	next := int64(0)
	push := func() {
		next++
		q.pushBack(fileSegment{fd: -1, offset: next})
	}
	want := int64(1)
	for round := 0; round < 100; round++ {
		push()
		push()
		if q.front().offset != want {
			t.Fatalf("round %d: front %d, want %d", round, q.front().offset, want)
		}
		q.popFront()
		want++
	}
	if q.len() != 100 || cap(q.segs) > 256 {
		t.Fatalf("queue of %d ranges holds %d slots", q.len(), cap(q.segs))
	}
	q.pushFront(fileSegment{fd: -1, offset: -1})
	if q.front().offset != -1 || q.back().offset != next {
		t.Fatalf("after pushFront: front %d, back %d", q.front().offset, q.back().offset)
	}
	for q.len() > 0 {
		q.popFront()
	}
	if q.head != 0 || len(q.segs) != 0 {
		t.Fatalf("an emptied queue kept head %d and %d slots in use", q.head, len(q.segs))
	}
	q.pushFront(fileSegment{fd: -1, offset: 7})
	if q.len() != 1 || q.front().offset != 7 {
		t.Fatal("pushFront into an empty queue")
	}
}

func socketPairForFiles(t *testing.T) (sender, receiver int) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
	})
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	return fds[0], fds[1]
}

func dupSegment(t *testing.T, f *os.File, remain int64) fileSegment {
	t.Helper()
	fd, err := socket.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return fileSegment{fd: fd, remain: remain}
}

// drainReceiver reads the receiving end until it ends and returns what came.
func drainReceiver(receiver int) <-chan []byte {
	got := make(chan []byte, 1)
	go func() {
		var out bytes.Buffer
		buf := make([]byte, 1<<20)
		for {
			n, err := syscall.Read(receiver, buf)
			if n <= 0 || err != nil {
				got <- out.Bytes()
				return
			}
			out.Write(buf[:n])
		}
	}()
	return got
}

// One flush sends no more than its budget of queued ranges, a step over at
// most, so a write turn hands on rather than streaming a large file alone.
func TestSendQueuedFilesStopsAtItsBudget(t *testing.T) {
	sender, receiver := socketPairForFiles(t)
	_ = syscall.SetsockoptInt(sender, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4<<20)
	_ = syscall.SetsockoptInt(receiver, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
	// A socket that takes less than the budget at once would stop the flush
	// at EAGAIN first, and the budget would go unmeasured.
	if sndbuf, err := syscall.GetsockoptInt(sender, syscall.SOL_SOCKET, syscall.SO_SNDBUF); err != nil || sndbuf < fileFlushBudget+sendfileStepMax {
		t.Skipf("the socket buffers %d bytes, too few to hold the budget", sndbuf)
	}
	got := drainReceiver(receiver)
	path := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(path, make([]byte, 8<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var c fdConn
	c.fd = int32(sender)
	zc := c.zeroCopyState()
	zc.files.pushBack(dupSegment(t, f, 8<<20))
	c.writeState.Or(writeFileTailFlag)
	c.pending.Store(8 << 20)
	n, more, err := c.sendQueuedFiles(fileFlushBudget)
	if err != nil || more {
		t.Fatalf("sendQueuedFiles = (%d, %v, %v)", n, more, err)
	}
	if n == 0 || n > fileFlushBudget+sendfileStepMax || zc.files.len() != 1 {
		t.Fatalf("one flush sent %d bytes and left %d ranges, want at most %d and the range still queued",
			n, zc.files.len(), fileFlushBudget+sendfileStepMax)
	}
	c.clearFilesLocked()
	_ = syscall.Shutdown(sender, syscall.SHUT_WR)
	<-got
}

// A file truncated after its range was queued fails the stream: the flush
// reports io.ErrUnexpectedEOF and marks the write failed, so nothing written
// after the range follows the part that went out.
func TestSendQueuedFilesFailsAtATruncatedFile(t *testing.T) {
	sender, _ := socketPairForFiles(t)
	f, err := os.CreateTemp(t.TempDir(), "trunc-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var c fdConn
	c.fd = int32(sender)
	zc := c.zeroCopyState()
	zc.files.pushBack(dupSegment(t, f, 64))
	zc.files.pushBack(dupSegment(t, f, 32))
	c.writeState.Or(writeFileTailFlag)
	c.pending.Store(96)
	if _, _, err := c.sendQueuedFiles(fileFlushBudget); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("sendQueuedFiles on an emptied file = %v, want io.ErrUnexpectedEOF", err)
	}
	if !c.writeFailed() {
		t.Fatal("a truncated range left the stream open to later bytes")
	}
	if n, err := c.flushWrite(); n != 0 || err != nil {
		t.Fatalf("flush after the failure = (%d, %v), want nothing sent", n, err)
	}
	c.clearFilesLocked()
}

// A withdrawal while a sendfile(2) reads the front range leaves the part that
// call reads queued, so what reaches the peer is exactly what the withdrawal
// did not count: a caller resuming at its count sends nothing twice.
func TestWithdrawingARangeBeingSentCountsWhatItSends(t *testing.T) {
	sender, receiver := socketPairForFiles(t)
	got := drainReceiver(receiver)
	path, data := writeTestFile(t, "sending", 4<<20)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var c fdConn
	c.fd = int32(sender)
	zc := c.zeroCopyState()
	seg := dupSegment(t, f, int64(len(data)))
	seg.owner = 7
	zc.files.pushBack(seg)
	c.writeState.Or(writeFileTailFlag)
	c.pending.Store(int64(len(data)))
	var withdrawn int64
	hook := func(*fdConn) {
		if withdrawn == 0 {
			withdrawn = c.cancelQueuedFiles(7)
		}
	}
	testHookQueuedSendfile.Store(&hook)
	defer testHookQueuedSendfile.Store(nil)
	deadline := time.Now().Add(10 * time.Second)
	for zc.files.len() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the part being sent never left")
		}
		if _, _, err := c.sendQueuedFiles(fileFlushBudget); err != nil {
			t.Fatal(err)
		}
		if c.writeBlocked() {
			c.setWriteBlocked(false)
			time.Sleep(time.Millisecond)
		}
	}
	_ = syscall.Shutdown(sender, syscall.SHUT_WR)
	sent := int64(len(data)) - withdrawn
	if b := <-got; int64(len(b)) != sent || !bytes.Equal(b, data[:sent]) {
		t.Fatalf("the peer got %d bytes, want the %d the withdrawal left (a prefix of the file)", len(b), sent)
	}
	if sent != sendfileStepMax {
		t.Fatalf("withdrawal left %d bytes, want the %d the call was reading", sent, sendfileStepMax)
	}
}
