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

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/petermattis/goid"
	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
	"github.com/urpc/uio/uws/internal/handshake"
)

// Conn is an established or handshaking WebSocket connection. UIO serializes
// protocol and handler callbacks per connection; atomics here coordinate
// external writers, timers, heartbeat scanning, and transport callbacks. The
// fields every message touches come first, so they share as few cache lines
// as the struct's size class allows.
type Conn struct {
	raw         uio.LayerDown
	config      *connConfig
	handler     Handler
	parser      *frame.Parser    // retained only while a frame spans input reads
	assembler   *frame.Assembler // retained only while a fragmented message is open
	compression *compressionState

	opened  atomic.Bool
	closed  atomic.Bool
	closing atomic.Bool

	readFrames int32 // frames delivered in the current read round
	// round is the read round's word, written only by the round and read by
	// senders: the mark of the goroutine running it (roundToken) while it
	// tells its replies apart (watchReplies), and roundBatching while it
	// batches the small frames it sends.
	round atomic.Uint32
	// readPauses holds independent automatic and application pause bits.
	// A transport drain clears only the automatic bit; ResumeReads clears
	// only the application's. See backpressure.go.
	readPauses atomic.Uint32

	batch *uio.Buffer // small frames accepted but not yet handed over; guarded by writes.mu
	// batchBuffered publishes the write batch's size for lock-free public
	// observations, including synchronous transport completion callbacks.
	// A negative value marks a batch being handed to the transport.
	batchBuffered atomic.Int64
	// replies follows the read rounds' replies while they are queued; nil
	// while none is followed. See replyLedger. A tracked transport — a layer
	// stack, TLS under the protocol — reports bytes on the wire, which are
	// not the frame bytes replies are counted in, so it tracks none.
	replies   atomic.Pointer[replyLedger]
	heartbeat *heartbeatState
	writes    connWriteState

	userData  any
	metadata  atomic.Pointer[connMetadata]
	handshake atomic.Pointer[handshakeState]
}

// setRaw installs the transport the connection runs on and records whether
// it hands out wire receipts (uio.TrackedWriter): with layers, the connection
// follows the Close frame and the heartbeat Ping by their receipts instead of
// counting bytes; a plain socket has no wire position to hand out and keeps
// the byte accounting. The bit rides the close flags so the connection keeps
// its size class.
func (c *Conn) setRaw(raw uio.LayerDown) {
	c.raw = raw
	if _, ok := raw.(uio.TrackedWriter); ok {
		c.writes.close.flags.Or(wireTrackedFlag)
	}
}

// wireTracked reports a transport that hands out wire receipts; see setRaw.
func (c *Conn) wireTracked() bool { return c.writes.close.flags.Load()&wireTrackedFlag != 0 }

// receiptSent is a tracked write's proof that the wire passed r. The Close
// frame and the heartbeat Ping are the tracked writes, and each takes its
// proof here: the Close frame lets the transport close be claimed, the Ping
// starts the Pong timeout.
func (c *Conn) receiptSent(r uio.SendReceipt) {
	if c.writes.close.claimReceipt(r) && c.writes.close.closePending() {
		_, _ = c.tryCloseTransport()
	}
	if heartbeat := c.heartbeat; heartbeat != nil && heartbeat.claimPingReceipt(r) {
		heartbeat.markPingReceiptSent(time.Now())
	}
}

// connWriteState serializes frame construction and submission. mu is held only
// while one message or control frame is encoded and handed to UIO, never
// across application code, so every sender may wait for it. close tracks
// progress that UIO's outbound callback updates without taking mu.
type connWriteState struct {
	mu    sync.Mutex
	close connCloseProgress
}

// These fields represent independent obligations rather than one phase: a
// Close frame can wait for outbound room while earlier bytes are still
// retiring. transitionMu only protects short state transitions; it never
// covers socket I/O or application work.
type connCloseProgress struct {
	pendingBytes atomic.Int64
	transitionMu sync.Mutex
	// aux carries the rare per-close state: the deferred Close frame's
	// payload until it is written, then the tracked frame's receipt. It
	// lives behind one pointer so the connection keeps its size class; only
	// the atomic done is read outside transitionMu.
	aux   *closeAux // guarded by transitionMu
	flags atomic.Uint32
}

// closeAux is one close's rare state: a Close frame waiting for outbound
// room, and later — once that frame is written — the tracked frame's place on
// the wire and whether the wire passed it. A deferred frame clears its
// payload as it is written, so the two never both hold data.
type closeAux struct {
	payload []byte          // a deferred Close frame; guarded by transitionMu
	receipt uio.SendReceipt // the tracked Close frame's position; guarded by transitionMu
	// earlySend keeps a receipt OnSent delivered before the writer registered
	// it: an external Close's tracked write returns from WriteTracked only
	// after a turn may have reported its receipt, and that report must not be
	// dropped; setReceipt consumes it. Guarded by transitionMu.
	earlySend uio.SendReceipt
	done      atomic.Bool // the wire passed the receipt
}

const (
	transportCloseIdle uint32 = iota
	transportClosePending
	transportCloseClaimed
)

const (
	closeFrameSent uint32 = 1 << iota
	// closeFrameDeferred marks a Close frame that waits for outbound room,
	// from the refusal until the frame is accepted or the transport aborts.
	closeFrameDeferred
)

// wireTrackedFlag marks a transport that hands out wire receipts; see setRaw.
// It sits above the transport phase bits (transportPhaseShift-Mask), which
// share this word.
const wireTrackedFlag uint32 = 1 << 4

const (
	transportPhaseShift = 2
	transportPhaseMask  = uint32(3 << transportPhaseShift)
)

// roundBatching marks a read round that batches the small frames it sends;
// the bits above it hold the round's goroutine mark.
const roundBatching uint32 = 1

// roundToken is the calling goroutine's mark in the round word: its id,
// shifted past roundBatching. The id is truncated, as UIO's write claims
// truncate it: two live goroutines would have to be two billion apart to
// share a mark. A goroutine whose mark is zero is never told apart, and its
// rounds count no replies.
func roundToken() uint32 { return uint32(goid.Get()) << 1 }

// countReply records a frame of wireSize bytes the read round's goroutine
// queued as one of the round's replies: the Handler answering what the round
// delivered, or the protocol answering a Ping. round is the round word its
// sender loaded, with the round's mark in it. A batched frame takes its place
// in the queue when the batch is handed over. Sends from anywhere else —
// another goroutine, another connection's round — are not replies, whatever
// they queue. It requires writes.mu.
func (c *Conn) countReply(round uint32, wireSize int, batched bool) {
	if !c.wireTracked() && round&^roundBatching == roundToken() {
		c.noteReply(wireSize, batched)
	}
}

// noteReply records a reply frame of wireSize bytes; see countReply. A
// batched reply is already appended, so its batch offset is available here.
// It requires writes.mu.
func (c *Conn) noteReply(wireSize int, batched bool) {
	if wireSize <= 0 || c.wireTracked() {
		return
	}
	replies := c.replies.Load()
	if replies == nil {
		if !batched && c.raw.OutboundBuffered() <= c.config.outboundLow {
			// A reply into a drained queue: the next round's check starts
			// the count over anyway.
			return
		}
		replies = newReplyLedger()
		c.replies.Store(replies)
	}
	if batched {
		replies.addBatched(int64(wireSize), int64(c.batch.Len()))
		return
	}
	replies.add(int64(wireSize), c.queueEnd(replies))
}

// placeReplyBatch counts the batched replies of a write batch just handed
// over. Each run keeps its offset within the batch, including gaps written
// by external senders. A refused batch queued nothing. Requires writes.mu.
func (c *Conn) placeReplyBatch(size int, accepted bool) {
	replies := c.replies.Load()
	if replies != nil && replies.batched != 0 {
		replies.placeBatch(int64(size), c.queueEnd(replies), accepted)
	}
}

// queueEnd is the position the transport's queue ends at, in the terms of
// replies: what it reported sent and what it still holds. The sent is read
// first: a send report retires the queue's bytes before it raises the sent
// count, so a report that lands between the two readings leaves the sum no
// higher than the truth — a reply placed short retires early, while one
// placed past its bytes would keep counting after it left. It requires
// writes.mu.
func (c *Conn) queueEnd(replies *replyLedger) int64 {
	sent := replies.sent.Load()
	return sent + int64(c.raw.OutboundBuffered())
}

// holdsReplies reports whether a read round stops consuming because its
// connection's replies back up: the queue reached the high watermark, and the
// replies still in it make up at least the band between the two watermarks.
// A connection that only answers pauses where its queue reaches the high
// watermark. Output queued from anywhere else — a broadcast, a pull Client's
// sends, a relay's forwards — never pauses reads: stopping them would not slow
// it, and would only keep the peer from being read.
func (c *Conn) holdsReplies(high int) bool {
	if c.wireTracked() {
		// A tracked transport reports wire bytes, not the frame bytes the
		// replies are counted in; see Conn.replies.
		return false
	}
	if c.internalReadsPaused() {
		return c.holdReads()
	}
	low := c.config.outboundLow
	queued := c.raw.OutboundBuffered()
	if queued <= low {
		c.watchReplies(false)
		if c.replies.Load() != nil {
			c.releaseReplies()
		}
		return false
	}
	c.watchReplies(true)
	replies := c.replies.Load()
	if queued < high || replies == nil {
		return false
	}
	c.writes.mu.Lock()
	own := replies.queued()
	c.writes.mu.Unlock()
	if own < int64(high-low) {
		return false
	}
	return c.holdReads()
}

// releaseReplies drops the ledger once every reply it followed has left, so
// a connection whose queue is drained keeps none. The drain's report asks
// for a round to run it: see resumeReadsAfterDrain.
func (c *Conn) releaseReplies() {
	c.writes.mu.Lock()
	if replies := c.replies.Load(); replies != nil && replies.batched == 0 && replies.queued() == 0 {
		c.replies.Store(nil)
	}
	c.writes.mu.Unlock()
}

// watchReplies publishes the round's mark in the round word, or withdraws
// it. Telling a send apart costs it a goroutine-id lookup, so a round tells
// its sends apart only while the queue holds more than the low watermark: the
// replies to a chunk read with the queue drained are not counted, and the
// count starts with the next chunk, once a check finds the queue above the low
// watermark — a connection that only answers pauses at most one chunk later
// than counting every reply would. A mark whose queue drains stays until the
// round's next check or its end.
func (c *Conn) watchReplies(on bool) {
	round := c.round.Load()
	if watched := round > roundBatching; watched == on {
		return
	}
	if on {
		c.round.Store(round | roundToken())
	} else {
		c.round.Store(round & roundBatching)
	}
}

// holdReads reports whether a read round stops consuming, so UIO's inbound
// hold stops the socket reads. A connection pauses once holdsReplies finds
// its replies backed up and stays paused until the queue drains to the low
// watermark, whatever runs its next round: input that arrives in between is
// left for later. It counts both transitions.
//
// The pause is published before the queue is looked at again, and
// resumeReadsAfterDrain reads the pause after the transport has retired the
// bytes it reports, so whichever of the two runs second sees the other: a
// queue that drained between the round's check and the pause is caught here,
// and the round goes on reading.
func (c *Conn) holdReads() bool {
	low := c.config.outboundLow
	if c.internalReadsPaused() {
		if c.raw.OutboundBuffered() > low {
			return true
		}
		// Drained: resume here. Losing the race to resumeReadsAfterDrain
		// means it resumed, and woke the connection, already.
		if c.setInternalReadPause(false) && c.config.stats != nil {
			c.config.stats.resumed.Add(1)
		}
		return false
	}
	if !c.setInternalReadPause(true) {
		return true
	}
	if c.raw.OutboundBuffered() <= low && c.setInternalReadPause(false) {
		return false
	}
	if c.config.stats != nil {
		c.config.stats.paused.Add(1)
	}
	return true
}

// resumeReadsAfterDrain wakes a paused connection once its queued output has
// fallen to the low watermark. The transport's outbound callback calls it.
// A connection that was never paused, or already resumed, gets a round only
// when it still follows replies: the round drops the ledger once the queue
// has drained (releaseReplies), which no other callback can run.
func (c *Conn) resumeReadsAfterDrain() {
	config := c.config
	if config == nil || config.outboundHigh <= 0 {
		return
	}
	if c.internalReadsPaused() {
		if c.raw.OutboundBuffered() > config.outboundLow {
			return
		}
		if c.setInternalReadPause(false) {
			if config.stats != nil {
				config.stats.resumed.Add(1)
			}
			_ = c.raw.Wake()
		}
		return
	}
	if c.replies.Load() != nil && c.raw.OutboundBuffered() <= config.outboundLow {
		_ = c.raw.Wake()
	}
}

// compressionState holds negotiated direction-specific RFC 7692 contexts.
type compressionState struct {
	encoder *compress.Encoder
	decoder *compress.Decoder
}

// heartbeatState distinguishes a Ping waiting to be accepted, queued for
// transport, and actually sent. The first failed enqueue keeps its deadline
// across retries; byte positions mark when the Ping leaves the transport queue.
// Every submission holds the connection's write lock, so accepted positions
// follow the transport's order.
type heartbeatState struct {
	mu sync.Mutex
	// sendStalledAt remains set across backpressure retries.
	sendStalledAt int64
	pingQueuedAt  int64
	pingSentAt    int64
	pingNonce     uint64
	pingTarget    uint64
	// nextPingAt is when the next Ping may be sent — zero until the heartbeat
	// begins — and armedAt the wake deadline the transport currently holds.
	// Both change only in the connection's turn, under this mutex with the
	// rest.
	nextPingAt int64
	armedAt    int64
	// Accepted and retired are monotonic FIFO byte positions, a plain
	// transport's: the ping target marks when its complete frame has left the
	// transport queue without requiring the connection's later writes to
	// drain first. A tracked transport follows the Ping by its wire receipt
	// instead — pingReceipt, zero while that Ping is not tracked — and the
	// counters stay still.
	outboundAccepted atomic.Uint64
	outboundRetired  atomic.Uint64
	pingOutstanding  atomic.Bool
	pingReceipt      uio.SendReceipt // guarded by mu
}

// closeTimerState protects lazy creation, replacement, and cancellation of the
// graceful-close deadline from callback and timer goroutines.
type closeTimerState struct {
	mu       sync.Mutex
	shard    uint8
	hasShard bool // the shard is picked once and then never changes
}

// connMetadata groups infrequently used negotiated and close information so a
// normal connection does not pay for separate synchronization fields.
type connMetadata struct {
	mu          sync.Mutex
	protocol    string
	closeErr    error
	closeCode   uint16
	closeReason string
	closeTimer  *closeTimerState // the graceful-close deadline's registration
	// User send reports made inside a batch handoff are delivered once the
	// handoff ends, so observers can see its final queue state.
	outboundDeferred atomic.Int64
	// Deferred reports may be delivered by the submitting goroutine rather
	// than the transport callback. Preserve serialization with other reports.
	outboundMu sync.Mutex
	// staged is a handshake's unconsumed frame tail. Only protocol read
	// turns access it, as they do the parser and assembler.
	staged []byte
}

// handshakeState contains every resource whose lifetime ends at handshake
// completion. notifyOpen releases the entire object after OnOpen returns, which
// also bounds the lifetime of an adopted net/http request and of a native
// upgrade request a handler may ask for during OnOpen.
type handshakeState struct {
	mu          sync.Mutex
	data        []byte
	upgrade     *httpUpgrade
	request     *handshake.Request // native upgrade request, exposed through Request()
	clientKey   string
	contextStop func() bool
	cleanup     func()
	epoch       uint64
	shard       uint8 // deadline shard the handshake timeout lives in
	shardSet    bool  // the shard is picked once and then never changes
	expired     bool
	pooled      bool // a native server handshake, recycled once OnOpen returns
}

// serverHandshake is a native server connection's handshake state together
// with the upgrade request it parses, allocated as one object and recycled
// once OnOpen returns, so accepting a connection costs neither of them.
type serverHandshake struct {
	state   handshakeState
	request handshake.Request
}

var serverHandshakes = sync.Pool{New: func() any { return new(serverHandshake) }}

// maxPooledHandshakeData bounds the gathered header bytes a recycled
// handshake keeps the buffer of: a split upgrade request is rare, and a large
// one is not worth holding on to.
const maxPooledHandshakeData = 4 << 10

func newServerHandshake() *handshakeState {
	h := serverHandshakes.Get().(*serverHandshake)
	h.state.pooled = true
	return &h.state
}

// requestSlot returns the request a parsed upgrade is kept in: the pooled
// handshake's own, or a new one.
func (state *handshakeState) requestSlot() *handshake.Request {
	if state.pooled {
		return &(*serverHandshake)(unsafe.Pointer(state)).request
	}
	return new(handshake.Request)
}

// recycle returns a pooled handshake its connection no longer publishes. A
// goroutine that loaded it before then — a handshake deadline, or a
// Conn.Request racing OnOpen's return — takes mu and finds its connection's
// handshake gone, and reads nothing else; mu itself is never reset.
func (state *handshakeState) recycle() {
	if !state.pooled {
		return
	}
	h := (*serverHandshake)(unsafe.Pointer(state))
	state.mu.Lock()
	data := state.data[:0]
	if cap(data) > maxPooledHandshakeData {
		data = nil
	}
	state.data = data
	state.upgrade = nil
	state.request = nil
	state.clientKey = ""
	state.contextStop = nil
	state.cleanup = nil
	state.epoch++
	state.shard, state.shardSet, state.expired = 0, false, false
	h.request = handshake.Request{}
	state.mu.Unlock()
	serverHandshakes.Put(h)
}

// LocalAddr returns the local transport address.
func (c *Conn) LocalAddr() net.Addr { return c.raw.LocalAddr() }

// RemoteAddr returns the peer transport address.
func (c *Conn) RemoteAddr() net.Addr { return c.raw.RemoteAddr() }

// IsClosed reports whether the connection has closed.
func (c *Conn) IsClosed() bool { return c.closed.Load() }

// SetDeadline forwards the transport read and write deadline. A zero value
// clears it.
func (c *Conn) SetDeadline(deadline time.Time) error {
	if c == nil || c.raw == nil {
		return ErrNotReady
	}
	if h, ok := c.handler.(*pullHandler); ok {
		h.client.setReadDeadline(deadline)
		return c.raw.SetWriteDeadline(deadline)
	}
	return c.raw.SetDeadline(deadline)
}

// SetReadDeadline forwards the transport read deadline. A zero value clears it.
func (c *Conn) SetReadDeadline(deadline time.Time) error {
	if c == nil || c.raw == nil {
		return ErrNotReady
	}
	// A pull client's turn cannot observe the transport's expiry while its
	// OnMessage waits on a full queue, so the client owns the deadline; see
	// Client.setReadDeadline.
	if h, ok := c.handler.(*pullHandler); ok {
		h.client.setReadDeadline(deadline)
		return nil
	}
	return c.raw.SetReadDeadline(deadline)
}

// SetWriteDeadline forwards the transport write deadline. A zero value clears it.
func (c *Conn) SetWriteDeadline(deadline time.Time) error {
	if c == nil || c.raw == nil {
		return ErrNotReady
	}
	return c.raw.SetWriteDeadline(deadline)
}

// SetNoDelay controls whether the underlying TCP connection uses Nagle's
// algorithm.
func (c *Conn) SetNoDelay(noDelay bool) error {
	if c == nil || c.raw == nil {
		return ErrNotReady
	}
	return c.raw.SetNoDelay(noDelay)
}

// Request returns the HTTP upgrade request during OnOpen for server
// connections, whether accepted by Server.Serve or adopted through
// Server.ServeHTTP. It returns nil afterward — handlers that need it beyond
// OnOpen must keep it — and for client connections. The request is read-only.
func (c *Conn) Request() *http.Request {
	if c == nil {
		return nil
	}
	return c.handshakeHTTPRequest(c.handshake.Load())
}

func (c *Conn) handshakeHTTPRequest(state *handshakeState) *http.Request {
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if c.handshake.Load() != state {
		return nil // OnOpen returned meanwhile; the state may be another's now
	}
	if state.upgrade != nil {
		return state.upgrade.request.HTTP
	}
	if state.request == nil {
		return nil
	}
	if state.request.HTTP == nil {
		// The native path parsed the request without net/http; build it from
		// the retained header block only when a handler asks for it.
		if request, err := state.request.BuildHTTP(); err == nil {
			state.request.HTTP = request
		}
	}
	return state.request.HTTP
}

func (c *Conn) isClient() bool { return c.config != nil && c.config.client }

func (c *Conn) frameParserConfig() *frame.ParserConfig {
	if c.config == nil {
		return nil
	}
	return &c.config.parser
}

func (c *Conn) utf8ValidationEnabled() bool {
	if c.config == nil {
		return true
	}
	return c.config.assembler.ValidateUTF8
}

// Userdata returns the user data associated with the connection. It is not
// safe for concurrent use from multiple goroutines.
func (c *Conn) Userdata() any { return c.userData }

// SetUserdata replaces the user data associated with the connection. It is not
// safe for concurrent use from multiple goroutines.
func (c *Conn) SetUserdata(value any) {
	c.userData = value
}

// TLSState reports the TLS connection state of a connection that runs over
// TLS — a wss one, or one whose stack has a TLS layer below the WebSocket
// protocol: what the handshake settled on, the server name the client asked
// for, the client's certificates.
func (c *Conn) TLSState() (tls.ConnectionState, bool) {
	if c.raw == nil {
		return tls.ConnectionState{}, false
	}
	layer, ok := uio.FindLayer[interface{ State() tls.ConnectionState }](c.raw)
	if !ok {
		return tls.ConnectionState{}, false
	}
	return layer.State(), true
}

// Subprotocol returns the negotiated WebSocket subprotocol, or an empty string.
func (c *Conn) Subprotocol() string {
	metadata := c.metadata.Load()
	if metadata == nil {
		return ""
	}
	metadata.mu.Lock()
	defer metadata.mu.Unlock()
	return metadata.protocol
}

func (c *Conn) maxFramePayload() uint64 {
	if c.config == nil || c.config.parser.MaxFramePayload == 0 {
		return DefaultMaxFramePayload
	}
	return c.config.parser.MaxFramePayload
}

func (c *Conn) maxMessageSizeInt() int {
	maxMessage := c.maxMessageSize()
	if maxMessage > uint64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(maxMessage)
}

func (c *Conn) maxMessageSize() uint64 {
	if c.config == nil || c.config.assembler.MaxMessage == 0 {
		return DefaultMaxMessageSize
	}
	return c.config.assembler.MaxMessage
}

func (c *Conn) maxOutbound() int {
	if c.config == nil {
		return 0
	}
	return c.config.maxOutbound
}

func (c *Conn) writeBufferedThreshold() int {
	if c.config == nil || c.config.writeBufferedThreshold == 0 {
		return defaultWriteBufferedThreshold
	}
	return c.config.writeBufferedThreshold
}

func (c *Conn) closeInfo() CloseEvent {
	metadata := c.metadata.Load()
	if metadata == nil {
		return CloseEvent{}
	}
	metadata.mu.Lock()
	defer metadata.mu.Unlock()
	return CloseEvent{Code: metadata.closeCode, Reason: metadata.closeReason, Err: metadata.closeErr}
}

func (c *Conn) ensureMetadata() *connMetadata {
	for {
		if metadata := c.metadata.Load(); metadata != nil {
			return metadata
		}
		metadata := &connMetadata{}
		if c.metadata.CompareAndSwap(nil, metadata) {
			return metadata
		}
	}
}

func (c *Conn) setSubprotocol(protocol string) {
	if protocol == "" {
		return
	}
	metadata := c.ensureMetadata()
	metadata.mu.Lock()
	metadata.protocol = protocol
	metadata.mu.Unlock()
}

func (c *Conn) setCloseReason(code uint16, reason string) {
	metadata := c.ensureMetadata()
	metadata.mu.Lock()
	metadata.closeCode = code
	metadata.closeReason = reason
	metadata.mu.Unlock()
}

func (c *Conn) setCloseError(err error) {
	metadata := c.ensureMetadata()
	metadata.mu.Lock()
	if metadata.closeErr == nil {
		metadata.closeErr = err
	}
	metadata.mu.Unlock()
}
