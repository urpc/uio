package uws

import (
	"runtime"
	"sync"
	"testing"

	"github.com/urpc/uio/uws/internal/frame"
)

// A small reply must keep counting even when every reply is separated from
// the next by an application send. Dropping older queue positions can leave
// the count below the watermark forever while the peer reads nothing.
func TestInterleavedSmallPongsEventuallyPauseReads(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	ping := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Ping, Masked: true}, [4]byte{1, 2, 3, 4})
	for range 200 {
		raw.inbound = append([]byte(nil), ping...)
		if err := conn.readAvailable(); err != nil {
			t.Fatal(err)
		}
		if conn.internalReadsPaused() {
			if stats := server.Stats(); stats.ReadsPaused != 1 {
				t.Fatalf("stats = %+v, want exactly one pause", stats)
			}
			return
		}
		// This send happens after the read round, so its two wire bytes are
		// output from elsewhere, separating the two-byte Pong replies.
		if err := conn.SendBinary(nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("small replies never paused reads: queued %d, own queued %d", raw.queuedBytes(), ownQueued(conn))
}

// An external writer can join a read round's batch between two replies.
// Sending the first reply must retire its bytes without making the external
// bytes between the replies count toward the pause.
func TestMixedBatchRetiresOnlyItsReplyBytes(t *testing.T) {
	raw, conn, _ := newBackpressureConn(64, 16)
	if err := conn.SendBinary(make([]byte, 18)); err != nil { // 20 wire bytes
		t.Fatal(err)
	}
	calls := 0
	conn.handler.(*batchEchoHandler).onMessage = func(c *Conn, _ Message) {
		calls++
		if calls == 1 {
			if err := c.SendBinary(make([]byte, 48)); err != nil { // 50 reply bytes
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- c.SendBinary(make([]byte, 98)) }() // 100 external bytes
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		} else if err := c.SendBinary(nil); err != nil { // two more reply bytes
			t.Fatal(err)
		}
	}
	raw.inbound = maskedFrames("one", "two")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || raw.queuedBytes() != 172 {
		t.Fatalf("callbacks %d, queued %d; want two callbacks and 172 bytes", calls, raw.queuedBytes())
	}
	// The initial backlog and the first reply left. Only the external
	// 100-byte frame and the two-byte final reply are still queued.
	drainTo(raw, conn, 102)
	if got := ownQueued(conn); got != 2 {
		t.Fatalf("own queued = %d, want only the final reply's two bytes", got)
	}
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.internalReadsPaused() {
		t.Fatal("external bytes between batched replies paused reads")
	}
}

type queuedReplyInterval struct {
	start int64
	end   int64
}

func queuedReplyBytes(intervals []queuedReplyInterval, sent int64) int64 {
	var total int64
	for _, interval := range intervals {
		if interval.end > sent {
			total += interval.end - max(interval.start, sent)
		}
	}
	return total
}

// Compare the ledger with a plain FIFO of accepted reply intervals. The
// schedule fills its initial storage, retires a prefix, then adds enough
// separated runs to grow after the storage has wrapped. Later schedules
// combine partial retirement, adjacent replies and reuse after full drains.
func TestReplyLedgerMatchesFIFOAcrossGrowthAndReuse(t *testing.T) {
	ledger := newReplyLedger()
	var intervals []queuedReplyInterval
	var accepted, sent int64
	check := func() {
		t.Helper()
		if got, want := ledger.queued(), queuedReplyBytes(intervals, sent); got != want {
			t.Fatalf("accepted %d, sent %d: own queued %d, want %d", accepted, sent, got, want)
		}
	}
	add := func(reply, external int64) {
		t.Helper()
		accepted += external
		intervals = append(intervals, queuedReplyInterval{start: accepted, end: accepted + reply})
		accepted += reply
		ledger.add(reply, accepted)
		check()
	}
	drain := func(position int64) {
		t.Helper()
		if position < sent || position > accepted {
			t.Fatalf("invalid drain position %d after %d sent of %d accepted", position, sent, accepted)
		}
		sent = position
		ledger.sent.Store(sent)
		check()
	}

	for i := range 8 {
		add(int64(4+i%3), int64(2+i))
	}
	drain(intervals[3].end)
	for i := range 20 {
		add(int64(2+i%5), int64(3+i%7))
	}
	drain(intervals[10].start + 1)
	for i := range 160 {
		add(int64(1+i%9), int64(1+i%13))
		if i%5 == 4 {
			old := intervals[len(intervals)-9]
			drain(old.start + (old.end-old.start)/2)
		}
		if i%11 == 0 {
			add(3, 0) // adjacent replies may share a run
		}
	}
	drain(accepted)
	for cycle := range 4 {
		for i := range 40 {
			add(int64(1+(cycle+i)%7), int64(1+i%4))
		}
		last := intervals[len(intervals)-1]
		drain(last.start)
		drain(accepted)
	}
}

// The external write can also be the tail of the batch. No suffix of that
// external frame becomes a reply when all the actual reply bytes have left.
func TestMixedBatchWithExternalTailHasNoRepliesAfterDrain(t *testing.T) {
	raw, conn, _ := newBackpressureConn(64, 16)
	if err := conn.SendBinary(make([]byte, 18)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	conn.handler.(*batchEchoHandler).onMessage = func(c *Conn, _ Message) {
		calls++
		if calls != 1 {
			return
		}
		if err := c.SendBinary(make([]byte, 48)); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- c.SendBinary(make([]byte, 98)) }()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	raw.inbound = maskedFrames("one", "two")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.queuedBytes() != 170 {
		t.Fatalf("queued %d, want 170 bytes", raw.queuedBytes())
	}
	drainTo(raw, conn, 100)
	if got := ownQueued(conn); got != 0 {
		t.Fatalf("own queued = %d, want none with only the external frame left", got)
	}
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.internalReadsPaused() {
		t.Fatal("the external tail of a batch paused reads")
	}
}

// Many external sends can join one batch. Preserve the exact FIFO positions
// through more groups than the ledger's initial storage, including partial
// sends that end inside either kind of frame.
func TestManyMixedBatchGroupsMatchFIFO(t *testing.T) {
	raw, conn, _ := newBackpressureConn(4096, 16)
	if err := conn.SendBinary(make([]byte, 18)); err != nil {
		t.Fatal(err)
	}
	var intervals []queuedReplyInterval
	position := int64(20)
	conn.handler.(*batchEchoHandler).onMessage = func(c *Conn, _ Message) {
		if err := c.SendBinary(make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
		intervals = append(intervals, queuedReplyInterval{start: position, end: position + 7})
		position += 7
		done := make(chan error, 1)
		go func() { done <- c.SendBinary(make([]byte, 9)) }()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		position += 11
	}
	messages := make([]string, 64)
	for i := range messages {
		messages[i] = "request"
	}
	raw.inbound = maskedFrames(messages...)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if got := int64(raw.queuedBytes()); got != position || len(intervals) != len(messages) {
		t.Fatalf("queued %d, reply groups %d; want %d bytes and %d groups", got, len(intervals), position, len(messages))
	}
	for _, left := range []int{int(position), int(position) - 23, int(position) - 50, int(position) / 2, 130, 17, 0} {
		drainTo(raw, conn, left)
		if got, want := ownQueued(conn), queuedReplyBytes(intervals, position-int64(left)); got != want {
			t.Fatalf("queue %d: own queued %d, want %d", left, got, want)
		}
	}
}

func TestReplyLedgerRandomFIFO(t *testing.T) {
	for seed := uint64(1); seed <= 16; seed++ {
		state := seed
		next := func() uint64 { state ^= state << 13; state ^= state >> 7; state ^= state << 17; return state }
		l := newReplyLedger()
		var intervals []queuedReplyInterval
		var accepted, sent int64
		for step := 0; step < 2000; step++ {
			switch next() % 5 {
			case 0:
				if accepted > sent {
					sent += int64(next() % uint64(accepted-sent+1))
					l.sent.Store(sent)
				}
			case 1:
				n := int64(1 + next()%31)
				accepted += int64(next() % 17)
				accepted += n
				intervals = append(intervals, queuedReplyInterval{accepted - n, accepted})
				l.add(n, accepted)
			default:
				var offset int64
				var pending []queuedReplyInterval
				for i, n := 0, int(next()%14)+1; i < n; i++ {
					offset += int64(next() % 19)
					size := int64(1 + next()%23)
					pending = append(pending, queuedReplyInterval{accepted + offset, accepted + offset + size})
					offset += size
					l.addBatched(size, offset)
				}
				offset += int64(next() % 19)
				acceptedBatch := next()%7 != 0
				end := accepted
				if acceptedBatch {
					end += offset
					intervals = append(intervals, pending...)
					accepted = end
				}
				l.placeBatch(offset, end, acceptedBatch)
			}
			if got, want := l.queued(), queuedReplyBytes(intervals, sent); got != want {
				t.Fatalf("seed %d step %d: queued %d, want %d", seed, step, got, want)
			}
		}
	}
}

func TestReleasePreservesPendingReplyBatch(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	raw.queued = 20
	round := roundToken() | roundBatching
	c.round.Store(round)
	if err := c.SendBinary(make([]byte, 48)); err != nil {
		t.Fatal(err)
	}
	ledger := c.replies.Load()
	if ledger == nil || pendingBatchReplies(c) != 50 {
		t.Fatalf("pending ledger %v batched %d", ledger != nil, pendingBatchReplies(c))
	}
	drainTo(raw, c, 0)
	c.releaseReplies()
	if c.replies.Load() != ledger {
		t.Fatal("release dropped the pending batch ledger")
	}
	c.endRound(round)
	if got := ownQueued(c); got != 50 {
		t.Fatalf("after handoff own %d, want 50", got)
	}
	drainTo(raw, c, 0)
	c.releaseReplies()
	if c.replies.Load() != nil {
		t.Fatal("drained ledger retained")
	}
}

type replyDrainFlushWire struct {
	*limitedWire
	conn *Conn
}

func (w *replyDrainFlushWire) Flush() error { drainTo(w.limitedWire, w.conn, 0); return nil }

func TestPongFlushDrainsEarlierReplyBatch(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	wire := &replyDrainFlushWire{limitedWire: raw, conn: c}
	c.raw = wire
	raw.queued = 20
	round := roundToken() | roundBatching
	c.round.Store(round)
	if err := c.SendBinary(make([]byte, 48)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.SendBinary(make([]byte, 98)) }()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := c.sendPong(nil); err != nil {
		t.Fatal(err)
	}
	c.endRound(round)
	if got := ownQueued(c); got != 0 {
		t.Fatalf("own %d after synchronous Flush drain, want 0", got)
	}
	if pendingBatchReplies(c) != 0 {
		t.Fatalf("batch count %d after Flush", pendingBatchReplies(c))
	}
	c.releaseReplies()
	if c.replies.Load() != nil {
		t.Fatal("drained ledger retained")
	}
}

// Sends and transport retirements can race with a read round. Once every
// earlier byte has left, an external tail must contain no recorded replies.
func TestConcurrentReplyCallbacksKeepExternalTailUnowned(t *testing.T) {
	raw, c, _ := newBackpressureConn(64<<10, 16<<10)
	raw.queued = 20 << 10
	c.handler.(*batchEchoHandler).onMessage = func(c *Conn, _ Message) {
		if err := c.SendBinary(make([]byte, 5)); err != nil {
			t.Error(err)
		}
	}
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < 2000; i++ {
			if err := c.SendBinary(make([]byte, 62)); err != nil {
				t.Error(err)
				return
			}
			if i%5 == 0 {
				runtime.Gosched()
			}
		}
	}()
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw.mu.Lock()
			n := min(raw.queued, 73)
			raw.queued -= n
			raw.mu.Unlock()
			c.transportSent(n)
			c.resumeReadsAfterDrain()
			runtime.Gosched()
		}
	}()
	var stopOnce sync.Once
	stopDrain := func() {
		stopOnce.Do(func() { close(stop) })
		<-stopped
	}
	t.Cleanup(func() { writers.Wait(); stopDrain() })
	for i := 0; i < 1000; i++ {
		raw.inbound = maskedFrames("one", "two")
		if err := c.readAvailable(); err != nil {
			t.Fatal(err)
		}
		if c.internalReadsPaused() {
			t.Fatal("less than 14 KiB of replies crossed the 48 KiB band")
		}
	}
	writers.Wait()
	stopDrain()
	before := raw.queuedBytes()
	if err := c.SendBinary(make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	drainTo(raw, c, raw.queuedBytes()-before)
	if got := ownQueued(c); got != 0 {
		t.Fatalf("own %d with only an external tail remaining", got)
	}
	c.handler.(*batchEchoHandler).onMessage = func(*Conn, Message) {}
	raw.inbound = maskedFrames("external tail")
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if c.internalReadsPaused() {
		t.Fatal("external tail paused reads after concurrent traffic")
	}
}
