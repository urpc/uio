package uws

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Handshake and close deadlines are kept in a sharded registry that a scanner
// goroutine sweeps, instead of one time.AfterFunc per connection. A timer and
// its closure cost the connection an allocation each plus the runtime's timer
// bookkeeping, and a churning connection pays it twice — once when it is
// accepted and once when it closes. Here a pending deadline is a map entry and
// a few stores; the scanner wakes at the earliest pending deadline, so a
// timeout still fires within about its scheduled moment, and at most one sweep
// interval late for a deadline armed while the scanner sleeps.
const (
	// deadlineShards spreads registration traffic across independent locks.
	deadlineShards = 16
	// deadlineMaxSweep bounds how long a newly armed deadline can wait for a
	// sweep; sooner deadlines wake the scanner directly.
	deadlineMaxSweep = time.Second

	deadlineKindHandshake uint8 = iota
	deadlineKindClose
)

type deadlineKey struct {
	conn *Conn
	kind uint8
}

type deadlineEntry struct {
	at    int64 // unixnano
	epoch uint64
}

type deadlineShard struct {
	mu       sync.Mutex
	pending  map[deadlineKey]deadlineEntry
	peak     int   // high-water of pending seen, for releasing map capacity
	nextWake int64 // unixnano the scanner will next look, 0 while it scans
	poke     chan struct{}
	started  bool
}

var deadlineSet [deadlineShards]deadlineShard

// deadlineShardIndex chooses a shard for a new deadline, round-robin.
var deadlineShardPick atomic.Uint32

func deadlineShardIndex() uint8 {
	return uint8(deadlineShardPick.Add(1) % deadlineShards)
}

func deadlineShardAt(index uint8) *deadlineShard { return &deadlineSet[index] }

// arm records a deadline for conn's kind. replace=false keeps an earlier
// pending deadline for the same pair, which is what ensureCloseTimer wants;
// every other caller replaces. epoch identifies the arming, so a cancel from
// an older arming cannot remove a newer one.
func (s *deadlineShard) arm(c *Conn, kind uint8, at int64, epoch uint64, replace bool) {
	s.mu.Lock()
	if s.pending == nil {
		s.pending = make(map[deadlineKey]deadlineEntry)
	}
	if !s.started {
		s.started = true
		s.poke = make(chan struct{}, 1)
		go s.run()
	}
	key := deadlineKey{c, kind}
	if _, pending := s.pending[key]; !pending || replace {
		s.pending[key] = deadlineEntry{at: at, epoch: epoch}
	}
	if len(s.pending) > s.peak {
		s.peak = len(s.pending)
	}
	poke := s.nextWake == 0 || at < s.nextWake
	s.mu.Unlock()
	if poke {
		select {
		case s.poke <- struct{}{}:
		default:
		}
	}
}

// cancel removes a deadline armed under epoch; a newer arming survives.
func (s *deadlineShard) cancel(c *Conn, kind uint8, epoch uint64) {
	s.mu.Lock()
	if entry, ok := s.pending[deadlineKey{c, kind}]; ok && entry.epoch == epoch {
		delete(s.pending, deadlineKey{c, kind})
	}
	s.mu.Unlock()
}

// has reports whether a deadline for the pair is still pending.
func (s *deadlineShard) has(c *Conn, kind uint8) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pending[deadlineKey{c, kind}]
	return ok
}

// run sweeps this shard until the process ends. Server lifecycle does not own
// it: deadlines may belong to connections of several servers, and each shard
// parks on a timer between sweeps.
func (s *deadlineShard) run() {
	timer := time.NewTimer(deadlineMaxSweep)
	defer timer.Stop()
	for {
		now := time.Now().UnixNano()
		var due []deadlineKey
		var epochs []uint64
		next := int64(0)
		s.mu.Lock()
		for key, entry := range s.pending {
			if entry.at <= now {
				due = append(due, key)
				epochs = append(epochs, entry.epoch)
				delete(s.pending, key)
				continue
			}
			if next == 0 || entry.at < next {
				next = entry.at
			}
		}
		wait := int64(deadlineMaxSweep)
		if next != 0 && next-now < wait {
			wait = next - now
		}
		s.nextWake = now + wait
		// Go maps never give capacity back, and a dial wave grows this one to
		// its peak within a second. Once that wave is over, start fresh so the
		// high-water mark is not held for the process lifetime.
		if len(s.pending) == 0 && s.peak > 64 {
			s.pending = make(map[deadlineKey]deadlineEntry)
			s.peak = 0
		}
		s.mu.Unlock()

		for i, key := range due {
			key.conn.fireDeadline(key.kind, epochs[i])
		}

		timer.Reset(time.Duration(wait))
		select {
		case <-timer.C:
		case <-s.poke:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		s.mu.Lock()
		s.nextWake = 0
		s.mu.Unlock()
	}
}

// fireDeadline runs one expired deadline. The handshake case carries the epoch
// it was armed with, so a re-armed handshake survives a deadline collected
// just before it; both cases then rely on the checks their old timer
// callbacks relied on.
func (c *Conn) fireDeadline(kind uint8, epoch uint64) {
	switch kind {
	case deadlineKindHandshake:
		state := c.handshake.Load()
		if state == nil {
			return
		}
		c.expireHandshake(state, epoch, context.DeadlineExceeded)
	case deadlineKindClose:
		if c.closed.Load() {
			return
		}
		c.abortTransport(io.ErrClosedPipe)
	}
}
