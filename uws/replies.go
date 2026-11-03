/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package uws

import "sync/atomic"

// replyRuns is the number of reply runs a ledger keeps without a second
// allocation. More separated replies grow the queue; none may be forgotten
// or merged across output from another sender.
const replyRuns = 8

// replyLedger follows the replies a connection's read rounds queued by where
// they sit in the transport's queue, so a reply stops counting once the
// transport reports it sent. A position counts bytes through the queue since
// the ledger began, what was queued then first: a byte at position p has left
// once p bytes were reported sent.
//
// Consecutive replies share a run; output from another sender separates
// runs. Keeping those gaps is necessary both to retire only replies that
// actually left and to count every reply toward the watermark. A read pause
// bounds the queued replies; storage follows their number of separate runs.
// Batched replies keep their offsets until the batch enters the transport.
//
// sent is added to by the transport's outbound callback, which takes no lock;
// everything else is guarded by the connection's writes.mu.
type replyLedger struct {
	sent   atomic.Int64 // bytes the transport reported sent since the ledger began
	inline [replyRuns]replyRun
	runs   []replyRun // a ring, in queue order from first
	first  int
	count  int
	total  int64
	// batch is the latest run in the current write batch. Only a batch
	// containing external bytes between replies needs batchMore.
	batch     replyRun
	batchMore []replyRun
	batched   int64
}

type replyRun struct {
	size int64
	end  int64
}

func newReplyLedger() *replyLedger {
	l := new(replyLedger)
	l.runs = l.inline[:]
	return l
}

// add records n bytes of replies whose last byte sits at end. Replies that
// continue the newest run join it; otherwise they open a run of their own.
// A position snapshot can be short of the actual queue end (queueEnd).
// Clip bytes already sent or recorded so that such a snapshot cannot count
// the same positions twice.
func (l *replyLedger) add(n, end int64) {
	if n <= 0 {
		return
	}
	sent := l.sent.Load()
	l.retire(sent)
	start := max(end-n, sent)
	if l.count > 0 {
		last := &l.runs[(l.first+l.count-1)%len(l.runs)]
		start = max(start, last.end)
		if end <= start {
			return
		}
		n = end - start
		if last.end == start {
			last.size += n
			last.end = end
			l.total += n
			return
		}
	}
	if end <= start {
		return
	}
	n = end - start
	if l.count == len(l.runs) {
		runs := make([]replyRun, 2*len(l.runs))
		for i := range l.count {
			runs[i] = l.runs[(l.first+i)%len(l.runs)]
		}
		l.runs, l.first = runs, 0
	}
	i := (l.first + l.count) % len(l.runs)
	l.runs[i] = replyRun{size: n, end: end}
	l.count++
	l.total += n
}

// addBatched records a reply by its offset in the write batch, including
// external bytes before or between replies. It does not enter the transport
// queue until placeBatch is called.
func (l *replyLedger) addBatched(n, end int64) {
	l.batched += n
	if l.batch.size != 0 && l.batch.end != end-n {
		l.batchMore = append(l.batchMore, l.batch)
		l.batch = replyRun{}
	}
	l.batch.size += n
	l.batch.end = end
}

// placeBatch translates batch offsets to transport positions. The batch's
// end is sampled once, so every run keeps its position relative to the others.
func (l *replyLedger) placeBatch(size, end int64, accepted bool) {
	if accepted {
		base := end - size
		for _, run := range l.batchMore {
			l.add(run.size, base+run.end)
		}
		l.add(l.batch.size, base+l.batch.end)
	}
	l.batch = replyRun{}
	l.batchMore = l.batchMore[:0]
	l.batched = 0
}

// retire drops the runs the transport has reported all of sent.
func (l *replyLedger) retire(sent int64) {
	for l.count > 0 && l.runs[l.first].end <= sent {
		l.total -= l.runs[l.first].size
		l.first = (l.first + 1) % len(l.runs)
		l.count--
	}
}

// queued reports the reply bytes still in the transport's queue.
func (l *replyLedger) queued() int64 {
	sent := l.sent.Load()
	l.retire(sent)
	if l.count == 0 {
		return 0
	}
	n := l.total
	first := l.runs[l.first]
	if left := first.end - sent; left < first.size {
		n -= first.size - left
	}
	return n
}
