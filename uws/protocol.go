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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/extension"
	"github.com/urpc/uio/uws/internal/frame"
	"github.com/urpc/uio/uws/internal/handshake"
)

// startHandshakeTimer advances the handshake epoch before installing a timer.
// Epoch checks make callbacks from replaced timers harmless.
func (c *Conn) startHandshakeTimer(timeout time.Duration) {
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}
	state := c.ensureHandshakeState()
	state.mu.Lock()
	state.epoch++
	epoch := state.epoch
	stopContext := state.contextStop
	state.contextStop = nil
	// The shard is picked once per connection: arming and canceling must agree,
	// and a second arm must land where the cancel can reach it.
	if !state.shardSet {
		state.shard = deadlineShardIndex()
		state.shardSet = true
	}
	shard := state.shard
	state.expired = false
	state.mu.Unlock()
	deadlineShardAt(shard).arm(c, deadlineKindHandshake, time.Now().Add(timeout).UnixNano(), epoch, true)
	if stopContext != nil {
		stopContext()
	}
}

// watchHandshakeContext links one dial attempt to the same epoch as its timer.
// Whichever completion path wins removes and stops the other callback.
func (c *Conn) watchHandshakeContext(ctx context.Context) {
	if ctx == nil || ctx.Done() == nil {
		return
	}
	state := c.handshake.Load()
	if state == nil {
		return
	}
	state.mu.Lock()
	epoch := state.epoch
	if state.expired || c.opened.Load() || c.closed.Load() {
		state.mu.Unlock()
		return
	}
	state.mu.Unlock()

	stop := context.AfterFunc(ctx, func() {
		c.expireHandshake(state, epoch, context.Cause(ctx))
	})
	state.mu.Lock()
	stale := c.handshake.Load() != state || epoch != state.epoch || state.expired || c.opened.Load() || c.closed.Load()
	var previous func() bool
	if !stale {
		previous = state.contextStop
		state.contextStop = stop
	}
	state.mu.Unlock()
	if previous != nil {
		previous()
	}
	if stale {
		stop()
	}
}

// expireHandshake wins only for the currently published state and epoch. It
// detaches all handshake resources before closing the transport so OnClose
// cannot observe half-cleaned state.
func (c *Conn) expireHandshake(state *handshakeState, epoch uint64, cause error) {
	if cause == nil {
		cause = context.Canceled
	}
	state.mu.Lock()
	if c.handshake.Load() != state || epoch != state.epoch || state.expired || c.opened.Load() || c.closed.Load() {
		state.mu.Unlock()
		return
	}
	state.expired = true
	resources := state.detachResourcesLocked(c)
	c.handshake.CompareAndSwap(state, nil)
	state.mu.Unlock()
	resources.stop()
	_ = c.requestClose(cause)
}

// stopHandshakeTimer tears down the complete handshake state on rejection or
// transport close, including dial context hooks and hijack cleanup.
func (c *Conn) stopHandshakeTimer() {
	state := c.handshake.Load()
	if state == nil {
		return
	}
	state.mu.Lock()
	state.expired = true
	resources := state.detachResourcesLocked(c)
	c.handshake.CompareAndSwap(state, nil)
	state.mu.Unlock()
	resources.stop()
}

// markOpened atomically transitions a live handshake to open and stops its
// timeout sources. The state itself remains published through OnOpen so
// Conn.Request can expose an adopted HTTP request during that callback.
func (c *Conn) markOpened() bool {
	state := c.handshake.Load()
	if state == nil {
		return false
	}
	state.mu.Lock()
	if state.expired || c.handshake.Load() != state || c.opened.Load() || c.closed.Load() {
		state.mu.Unlock()
		return false
	}
	resources := state.detachResourcesLocked(c)
	c.opened.Store(true)
	state.mu.Unlock()
	resources.stop()
	return true
}

type handshakeResources struct {
	conn        *Conn
	shard       uint8
	epoch       uint64
	contextStop func() bool
	cleanup     func()
}

// detachResourcesLocked invalidates timer/context callbacks while ownership of
// the handshake state is still serialized by its mutex.
func (state *handshakeState) detachResourcesLocked(c *Conn) handshakeResources {
	epoch := state.epoch
	state.epoch = epoch + 1
	resources := handshakeResources{
		conn:        c,
		shard:       state.shard,
		epoch:       epoch,
		contextStop: state.contextStop,
		cleanup:     state.cleanup,
	}
	state.contextStop = nil
	state.cleanup = nil
	return resources
}

func (resources handshakeResources) stop() {
	deadlineShardAt(resources.shard).cancel(resources.conn, deadlineKindHandshake, resources.epoch)
	if resources.contextStop != nil {
		resources.contextStop()
	}
	if resources.cleanup != nil {
		resources.cleanup()
	}
}

func (c *Conn) ensureHandshakeState() *handshakeState {
	for {
		if state := c.handshake.Load(); state != nil {
			return state
		}
		state := &handshakeState{}
		if c.handshake.CompareAndSwap(nil, state) {
			return state
		}
	}
}

func (c *Conn) releaseHandshakeState(state *handshakeState) {
	if state != nil {
		c.handshake.CompareAndSwap(state, nil)
	}
}

// readAvailable consumes borrowed UIO inbound chunks without copying complete
// frames. It yields after the connection's frame budget for one chunk so a
// hot connection returns to the scheduler and cannot monopolize one worker.
// A round that carries several frames batches the small replies they send and
// hands them to the transport together when it ends.
func (c *Conn) readAvailable() error {
	err := c.readRound()
	if c.batching.Load() {
		c.endWriteBatch()
	}
	c.heartbeatTick(time.Now())
	return err
}

func (c *Conn) readRound() error {
	c.readFrames = 0
	high := 0
	if c.config != nil {
		high = c.config.outboundHigh
	}
	for {
		// Queued output at the high watermark, or a pause that has not
		// drained to the low watermark: stop consuming, so the bytes build up
		// in UIO's inbound and its hold stops the socket reads.
		if high > 0 && (c.readPaused.Load() || c.raw.OutboundBuffered() >= high) && c.holdReads() {
			return nil
		}
		data := c.raw.PeekChunk()
		if len(data) == 0 {
			return nil
		}
		if !c.opened.Load() {
			err := c.consumeHandshake(data)
			if err != nil {
				_, _ = c.raw.Discard(len(data))
				return c.rejectHandshake(err)
			}
			_, _ = c.raw.Discard(len(data))
			continue
		}

		consumed, err := c.consumeFrames(data)
		if consumed > 0 {
			_, _ = c.raw.Discard(consumed)
		}
		if err != nil {
			if err == errReadBudget || errors.Is(err, errReadBudget) {
				if yieldErr := c.raw.YieldRead(); yieldErr != nil {
					return fmt.Errorf("uws: yield buffered read: %w", yieldErr)
				}
				return nil
			}
			if errors.Is(err, ErrClosed) {
				// This end's transport is closing or closed: stop consuming
				// instead of treating it as a protocol error, which would
				// close the transport before the peer completes the close
				// handshake.
				return nil
			}
			return c.protocolClose(err)
		}
		if consumed == 0 {
			return nil
		}
	}
}

// connFrameSink delivers a read round's frames to its connection and charges
// them against the round's budget. It is the Conn itself, so handing it to the
// incremental parser as a frame.Sink allocates nothing.
type connFrameSink Conn

func (sink *connFrameSink) AcceptFrame(f frame.Frame) error {
	c := (*Conn)(sink)
	if err := c.acceptFrame(f); err != nil {
		return err
	}
	return c.countFrame()
}

// consumeShortFrame delivers the frame a server reads most: complete, masked,
// unfragmented text or binary of seven-bit length, or of 16-bit length within
// the frame and message limits, with no message being reassembled. It makes
// ParseFrame's and acceptFrame's checks for that one shape directly,
// unmasking in place, and reports false, having consumed nothing, for any
// other frame or a frame that is not complete yet. When more input follows
// the frame, the round's replies are batched.
//
// limit is the connection's shortReadLimit: the largest seven-bit payload
// within the frame and message limits, or -1 when frames are not expected
// masked.
func (c *Conn) consumeShortFrame(src []byte, limit int) (int, bool, error) {
	if len(src) < 6 || c.assembler != nil {
		return 0, false, nil
	}
	b0, b1 := src[0], src[1]
	if b0-(0x80|byte(frame.Text)) > 1 || b1&0x80 == 0 {
		return 0, false, nil
	}
	n := int(b1 &^ 0x80)
	head := 2
	if n == 126 {
		// A 16-bit length, one header step wider at the same fused cost: the
		// length moves the mask key and payload two bytes on. The frame and
		// message limits gate this branch, where the seven-bit one is
		// bounded by its encoding. 64-bit lengths keep the generic path.
		if limit < 0 || len(src) < 8 {
			return 0, false, nil
		}
		n = int(binary.BigEndian.Uint16(src[2:4]))
		head = 4
		// A length under 126 encoded in 16 bits is not canonical: the
		// generic parser refuses it with a protocol error.
		if n < 126 || (c.config.parser.MaxFramePayload != 0 && uint64(n) > c.config.parser.MaxFramePayload) ||
			(c.config.assembler.MaxMessage != 0 && uint64(n) > c.config.assembler.MaxMessage) {
			return 0, false, nil
		}
	} else if n > limit {
		return 0, false, nil
	}
	if len(src) < head+4+n {
		return 0, false, nil
	}
	size := head + 4 + n
	payload := src[head+4 : size : size]
	// From VectorUnmaskMin bytes the frame package's vector unmask beats the
	// fused word loop: a binary payload needs no ASCII verdict for the UTF-8
	// check below, and a text payload takes it from the same pass.
	ascii := false
	switch {
	case n < frame.VectorUnmaskMin:
		ascii = unmaskShort(payload, binary.LittleEndian.Uint32(src[head:head+4]))
	case b0 == 0x80|byte(frame.Binary):
		frame.Unmask(payload, [4]byte(src[head:head+4]))
	default:
		ascii = frame.UnmaskASCII(payload, [4]byte(src[head:head+4]))
	}
	if size < len(src) {
		c.beginWriteBatch()
	}
	if c.closed.Load() {
		return size, true, ErrClosed
	}
	opcode := frame.OpCode(b0 & 0x0f)
	if opcode == frame.Text && !ascii && c.config.assembler.ValidateUTF8 && !frame.ValidUTF8(payload) {
		return size, true, frame.ErrInvalidUTF8
	}
	// notifyMessage, inline: this runs for every frame of a pipelined round.
	// A close in progress drops the data without a callback, so the round
	// keeps consuming and the peer's Close still completes the handshake.
	if c.closing.Load() {
		return size, true, nil
	}
	if handler := c.handler; handler != nil {
		handler.OnMessage(c, Message{Type: messageType(opcode), Payload: payload})
	}
	return size, true, nil
}

// unmaskShort unmasks a short payload in place a word at a time, key being the
// four mask bytes read little-endian, and reports whether the result is all
// ASCII, which settles a text payload's UTF-8 check in the same pass.
func unmaskShort(payload []byte, key uint32) bool {
	key64 := uint64(key) | uint64(key)<<32
	var seen uint64
	for len(payload) >= 8 {
		word := binary.LittleEndian.Uint64(payload) ^ key64
		binary.LittleEndian.PutUint64(payload, word)
		seen |= word
		payload = payload[8:]
	}
	if len(payload) >= 4 {
		word := binary.LittleEndian.Uint32(payload) ^ key
		binary.LittleEndian.PutUint32(payload, word)
		seen |= uint64(word)
		payload = payload[4:]
	}
	for i := range payload {
		b := payload[i] ^ byte(key)
		key >>= 8
		payload[i] = b
		seen |= uint64(b)
	}
	return seen&0x8080808080808080 == 0
}

// countFrame charges a delivered frame against the round's budget.
func (c *Conn) countFrame() error {
	c.readFrames++
	frameBudget := int32(minFramesPerDataEvent)
	if c.config != nil && c.config.maxFramesPerDataEvent > 0 {
		frameBudget = int32(c.config.maxFramesPerDataEvent)
	}
	if c.readFrames >= frameBudget {
		return errReadBudget
	}
	return nil
}

// consumeFrames is feedFrames for a read round: complete frames are parsed in
// place and delivered without an intermediate callback, and only a frame that
// spans input chunks takes a pooled incremental parser. That parser finishes
// the one frame it holds and no more: a pipelining peer keeps several batches
// in flight, so most reads of a busy round start inside a frame, and the
// frames after it belong on the paths below like any others.
func (c *Conn) consumeFrames(data []byte) (int, error) {
	sink := (*connFrameSink)(c)
	consumed := 0
	if c.parser != nil {
		c.beginWriteBatch()
		n, err := c.parser.FinishFrame(data, sink)
		consumed = n
		if err != nil || c.parser.AtFrameBoundary() {
			releaseIncrementalParser(c.parser)
			c.parser = nil
		}
		if err != nil || c.parser != nil {
			return consumed, err
		}
	}
	cfg := c.frameParserConfig()
	limit := -1
	if c.config != nil {
		limit = c.config.shortReadLimit
	}
	for consumed < len(data) {
		if size, ok, err := c.consumeShortFrame(data[consumed:], limit); ok {
			consumed += size
			if err == nil {
				err = c.countFrame()
			}
			if err != nil {
				return consumed, err
			}
			continue
		}
		parsed, size, complete, err := frame.ParseFrame(data[consumed:], cfg)
		if err != nil {
			return consumed, err
		}
		if !complete {
			c.beginWriteBatch()
			parser := acquireIncrementalParser(cfg)
			n, err := parser.FeedTo(data[consumed:], sink)
			consumed += n
			if err != nil || parser.AtFrameBoundary() {
				releaseIncrementalParser(parser)
			} else {
				c.parser = parser
			}
			return consumed, err
		}
		consumed += size
		if consumed < len(data) {
			// More input follows this frame: batch the round's replies.
			c.beginWriteBatch()
		}
		if err := sink.AcceptFrame(parsed); err != nil {
			return consumed, err
		}
	}
	return consumed, nil
}

// Complete frames bypass this pool; only state spanning input reads is stored.
var incrementalParserPool sync.Pool
var fragmentedMessagePool sync.Pool

func acquireIncrementalParser(cfg *frame.ParserConfig) *frame.Parser {
	parser, _ := incrementalParserPool.Get().(*frame.Parser)
	if parser == nil {
		parser = &frame.Parser{}
	}
	parser.Init(cfg)
	return parser
}

func releaseIncrementalParser(parser *frame.Parser) {
	if parser == nil {
		return
	}
	parser.Init(nil)
	incrementalParserPool.Put(parser)
}

func acquireMessageAssembler(cfg *frame.AssemblerConfig) *frame.Assembler {
	assembler, _ := fragmentedMessagePool.Get().(*frame.Assembler)
	if assembler == nil {
		assembler = &frame.Assembler{}
	}
	assembler.Init(cfg)
	return assembler
}

func releaseMessageAssembler(assembler *frame.Assembler) {
	if assembler == nil {
		return
	}
	assembler.Init(nil)
	fragmentedMessagePool.Put(assembler)
}

// feedFrames keeps the zero-allocation ParseFrame path for complete frames and
// acquires a stateful parser only when a frame spans inbound chunks.
func (c *Conn) feedFrames(data []byte, emit func(frame.Frame) error) (int, error) {
	if c.parser != nil {
		consumed, err := c.parser.Feed(data, emit)
		if err != nil || c.parser.AtFrameBoundary() {
			releaseIncrementalParser(c.parser)
			c.parser = nil
		}
		return consumed, err
	}

	consumed := 0
	cfg := c.frameParserConfig()
	for consumed < len(data) {
		parsed, size, complete, err := frame.ParseFrame(data[consumed:], cfg)
		if err != nil {
			return consumed, err
		}
		if !complete {
			parser := acquireIncrementalParser(cfg)
			n, err := parser.Feed(data[consumed:], emit)
			consumed += n
			if err != nil || parser.AtFrameBoundary() {
				releaseIncrementalParser(parser)
			} else {
				c.parser = parser
			}
			return consumed, err
		}
		consumed += size
		if err := emit(parsed); err != nil {
			return consumed, err
		}
	}
	return consumed, nil
}

func (c *Conn) releaseParser() {
	if c.parser == nil {
		return
	}
	releaseIncrementalParser(c.parser)
	c.parser = nil
}

// consumeHandshake incrementally accumulates the bounded HTTP header, installs
// negotiated compression, writes the server response, and then feeds any
// bytes received after the header as WebSocket frames.
func (c *Conn) consumeHandshake(data []byte) error {
	state := c.handshake.Load()
	if state == nil {
		return ErrClosed
	}
	if c.isClient() {
		state.data = append(state.data, data...)
		err := c.consumeClientHandshake()
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return err
	}
	// A request that arrived in one read is parsed where it lies: the read
	// round discards it only after this returns, and OnOpen, the last user of
	// the request, runs before. Only a request split across reads is gathered
	// into state.data.
	input := data
	if len(state.data) > 0 {
		state.data = append(state.data, data...)
		input = state.data
	}
	req, consumed, err := handshake.ParseServerRequest(input, handshake.ServerOptions{
		MaxHeaderBytes: c.config.maxHeaderBytes,
		CheckOrigin:    c.config.checkOrigin,
	})
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			if len(state.data) == 0 {
				state.data = append(state.data, data...)
			}
			return nil
		}
		return err
	}
	if authorize := c.config.authorize; authorize != nil {
		request, err := req.BuildHTTP()
		if err != nil {
			return err
		}
		if request.RemoteAddr == "" {
			request.RemoteAddr = c.raw.RemoteAddr().String()
		}
		if state, ok := c.TLSState(); ok {
			request.TLS = &state
		}
		req.HTTP = request
		userdata, err := authorize(request)
		if err != nil {
			return &authorizeError{err}
		}
		c.userData = userdata
	}
	// Publish the request for Conn.Request during OnOpen; notifyOpen releases
	// the state when the callback returns.
	state.mu.Lock()
	if c.handshake.Load() == state {
		state.request = &req
	}
	state.mu.Unlock()
	protocol := handshake.SelectSubprotocol(req.Subprotocols, c.config.subprotocols)
	params, extensions, err := extension.NegotiateServerWithPolicy(req.Extensions, c.config.compressionEnabled, !c.config.compressionContextTakeover)
	if err != nil {
		return err
	}
	params.Level = c.config.compressionLevel
	if params.Enabled {
		c.compression = &compressionState{
			encoder: compress.NewEncoderWithWindow(params.Level, params.ServerNoContextTakeover, params.ServerMaxWindowBits),
			decoder: compress.NewDecoderWithWindow(params.ClientNoContextTakeover, params.ClientMaxWindowBits),
		}
	}
	responseSize := handshake.ServerResponseSize(protocol, extensions)
	response := uio.AcquireBuffer(responseSize)
	wire := handshake.AppendServerResponse(response.AvailableBuffer()[:0], req, protocol, extensions)
	response.CommitWrite(len(wire))
	if err := c.writeTransportOwned(response); err != nil {
		return err
	}
	if err := c.raw.Flush(); err != nil {
		return err
	}
	c.setSubprotocol(protocol)
	if !c.markOpened() {
		return context.DeadlineExceeded
	}
	if c.heartbeat != nil {
		c.beginHeartbeat()
	}
	extra := append([]byte(nil), input[consumed:]...)
	c.notifyOpen()
	if len(extra) > 0 {
		return c.feedBehindHandshake(extra)
	}
	return nil
}

// feedBehindHandshake takes the frames that arrived behind a handshake. A
// plain Conn's OnOpen ran inside notifyOpen, so they are fed now. A Conn
// running as a layer opens the layers above only on the turn's open stage,
// after this callback returned — so the layer held the frames, and feeds
// them in the round asked for after it; see wsLayerHandler.
func (c *Conn) feedBehindHandshake(extra []byte) error {
	if sink, ok := c.handler.(layerSink); ok {
		sink.w.staged = append(sink.w.staged, extra...)
		return nil
	}
	if _, err := c.feedFrames(extra, c.acceptFrame); err != nil {
		if errors.Is(err, ErrClosed) {
			// OnOpen closed this end: stop consuming rather than treat it as
			// a protocol error, which would end the transport before the peer
			// answers the close handshake.
			return nil
		}
		return c.protocolClose(err)
	}
	return nil
}

// consumeClientHandshake validates and parses the response once, negotiates
// extensions from that result, and preserves post-header frame bytes.
func (c *Conn) consumeClientHandshake() error {
	state := c.handshake.Load()
	if state == nil {
		return ErrClosed
	}
	consumed, protocol, extensions, err := handshake.ValidateClientResponse(state.data, state.clientKey, c.config.subprotocols, c.config.maxHeaderBytes)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		if refused := clientHandshakeError(err); refused != err {
			return refused
		}
		return fmt.Errorf("uws: validate server handshake: %w", err)
	}
	params, err := extension.NegotiateClient(extensions, c.config.compressionEnabled)
	if err != nil {
		return err
	}
	params.Level = c.config.compressionLevel
	if params.Enabled {
		c.compression = &compressionState{
			encoder: compress.NewEncoderWithWindow(params.Level, params.ClientNoContextTakeover, params.ClientMaxWindowBits),
			decoder: compress.NewDecoderWithWindow(params.ServerNoContextTakeover, params.ServerMaxWindowBits),
		}
	}
	c.setSubprotocol(protocol)
	if !c.markOpened() {
		return context.DeadlineExceeded
	}
	if c.heartbeat != nil {
		c.beginHeartbeat()
	}
	extra := append([]byte(nil), state.data[consumed:]...)
	c.notifyOpen()
	if len(extra) > 0 {
		return c.feedBehindHandshake(extra)
	}
	return nil
}

// rejectHandshake ends a connection whose handshake failed with err. A
// server answers with the refusal err calls for (see rejectResponse) and
// closes once the answer has left: the response is counted like any frame,
// so the graceful close waits for it instead of dropping it. The returned
// error is the one the read round reports.
func (c *Conn) rejectHandshake(err error) error {
	if c.opened.Load() || c.closed.Load() {
		return err
	}
	if c.isClient() {
		_ = c.requestClose(err)
		return err
	}
	response := rejectResponse(err)
	buffer := uio.AcquireBuffer(len(response))
	_, _ = buffer.Write(response)
	if writeErr := c.writeTransportOwned(buffer); writeErr != nil {
		return err
	}
	if closeErr := c.closeTransport(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return errors.Join(err, closeErr)
	}
	return &ownedProtocolClose{cause: err}
}

// requestClose asks this end's connection to close. A plain Conn closes its
// transport; one running as a layer asks the stack, so the Close frame its
// layer says and the drain the stack waits for run as one close from this
// layer. A graceful cause closes gracefully, anything else aborts.
func (c *Conn) requestClose(cause error) error {
	if sink, ok := c.handler.(layerSink); ok {
		if cause == nil || errors.Is(cause, io.EOF) || errors.Is(cause, io.ErrUnexpectedEOF) {
			return sink.w.ctx.Close(cause)
		}
		return sink.w.ctx.Abort(cause)
	}
	if closer, ok := c.raw.(uio.Conn); ok {
		return closer.CloseWith(cause)
	}
	return net.ErrClosed
}

// closeTransport requests graceful transport shutdown. The raw connection
// closes once a deferred Close frame went out and every accepted byte retired,
// bounded by the close timer.
func (c *Conn) closeTransport() error {
	c.markClosing()
	if !c.writes.close.requestTransportClose() {
		return nil
	}
	if err := c.flush(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	if closed, err := c.tryCloseTransport(); closed {
		return err
	}
	c.ensureCloseTimer()
	_, err := c.tryCloseTransport()
	return err
}

func (c *Conn) closeTimeout() time.Duration {
	if c.config == nil || c.config.closeTimeout <= 0 {
		return DefaultCloseTimeout
	}
	return c.config.closeTimeout
}

// armCloseTimerLocked arms the connection's close timeout in its single shard.
// The state mutex stays held across the registry update so an arm and a cancel
// from another goroutine cannot interleave into a lost or duplicated entry.
func (c *Conn) armCloseTimerLocked(state *closeTimerState, replace bool) {
	if !state.hasShard {
		state.shard = deadlineShardIndex()
		state.hasShard = true
	}
	deadlineShardAt(state.shard).arm(c, deadlineKindClose, time.Now().Add(c.closeTimeout()).UnixNano(), 0, replace)
}

func (c *Conn) startCloseTimer() {
	if c.closed.Load() {
		return
	}
	state := c.ensureCloseTimerState()
	state.mu.Lock()
	defer state.mu.Unlock()
	if c.closed.Load() {
		return
	}
	c.armCloseTimerLocked(state, true)
}

func (c *Conn) ensureCloseTimer() {
	if c.closed.Load() {
		return
	}
	state := c.ensureCloseTimerState()
	state.mu.Lock()
	defer state.mu.Unlock()
	if c.closed.Load() {
		return
	}
	c.armCloseTimerLocked(state, false)
}

// tryCloseTransport claims shutdown once a deferred Close frame went out and
// all accepted wire bytes have completed. The claim is terminal even before
// UIO reports OnClose, so another close request cannot close the transport
// again.
func (c *Conn) tryCloseTransport() (bool, error) {
	if c.writes.close.closePending() {
		return c.closePendingTransport()
	}
	return false, nil
}

func (c *Conn) closePendingTransport() (bool, error) {
	if !c.writes.close.claimPendingTransportClose() {
		return false, nil
	}
	c.stopCloseTimer()
	return true, c.requestClose(io.EOF)
}

// abortTransport claims the same terminal responsibility as a graceful drain.
// A timer, a broken stream, and a protocol error may race; only one reaches
// UIO.
func (c *Conn) abortTransport(err error) {
	if !c.writes.close.claimAbort() {
		return
	}
	c.stopCloseTimer()
	c.releasePullClient()
	_ = c.requestClose(err)
}

// markClosing records that this end is closing the connection: messages
// that arrive meanwhile are dropped, and a pull Client's full queue no longer
// holds the close up.
func (c *Conn) markClosing() {
	c.closing.Store(true)
	c.releasePullClient()
}

func (c *Conn) stopCloseTimer() {
	state := c.closeTimer.Load()
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.hasShard {
		return
	}
	deadlineShardAt(state.shard).cancel(c, deadlineKindClose, 0)
}

func (c *Conn) ensureCloseTimerState() *closeTimerState {
	for {
		if state := c.closeTimer.Load(); state != nil {
			return state
		}
		state := &closeTimerState{}
		if c.closeTimer.CompareAndSwap(nil, state) {
			return state
		}
	}
}

// acceptFrame bypasses assembler allocation for complete messages and control
// frames. Fragmented state is pooled and released at every message boundary or
// error.
func (c *Conn) acceptFrame(f frame.Frame) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if c.assembler == nil {
		if f.Fin && !f.RSV1 && (f.Opcode == frame.Binary || f.Opcode == frame.Text) {
			// A complete uncompressed message, the common case: the checks
			// frame.AcceptSingle makes for it, without its callbacks.
			cfg := &c.config.assembler
			if cfg.MaxMessage != 0 && uint64(len(f.Payload)) > cfg.MaxMessage {
				return frame.ErrMessageTooBig
			}
			if f.Opcode == frame.Text && cfg.ValidateUTF8 && !frame.ValidUTF8(f.Payload) {
				return frame.ErrInvalidUTF8
			}
			return c.notifyMessage(Message{Type: messageType(f.Opcode), Payload: f.Payload})
		}
		if frame.IsControl(f.Opcode) || ((f.Opcode == frame.Text || f.Opcode == frame.Binary) && f.Fin) {
			return frame.AcceptSingle(f, &c.config.assembler, c.acceptControl, c.acceptMessage)
		}
	}
	if c.assembler == nil {
		c.assembler = acquireMessageAssembler(&c.config.assembler)
	}
	assembler := c.assembler
	err := assembler.Accept(f, c.acceptControl, c.acceptMessage)
	if err != nil || assembler.AtMessageBoundary() {
		c.releaseAssembler()
	}
	return err
}

func (c *Conn) releaseAssembler() {
	if c.assembler == nil {
		return
	}
	releaseMessageAssembler(c.assembler)
	c.assembler = nil
}

// acceptMessage delivers a borrowed payload. Compressed messages are expanded
// under MaxMessageSize before UTF-8 validation and callback delivery.
func (c *Conn) acceptMessage(message frame.Message) error {
	if message.Compressed {
		if c.compression == nil || c.compression.decoder == nil {
			return frame.ErrProtocol
		}
		return c.compression.decoder.DecodeBorrowed(message.Payload, c.maxMessageSizeInt(), func(payload []byte) error {
			if message.Opcode == frame.Text && c.utf8ValidationEnabled() && !frame.ValidUTF8(payload) {
				return frame.ErrInvalidUTF8
			}
			return c.notifyMessage(Message{Type: messageType(message.Opcode), Payload: payload})
		})
	}
	return c.notifyMessage(Message{Type: messageType(message.Opcode), Payload: message.Payload})
}

// acceptControl handles Ping/Pong/Close inline with frame order. Pong nonces
// must match the outstanding heartbeat; arbitrary Pong frames do not reset it.
func (c *Conn) acceptControl(f frame.Frame) error {
	switch f.Opcode {
	case frame.Ping:
		err := c.sendFrame(frame.Frame{Fin: true, Opcode: frame.Pong, Payload: f.Payload})
		if errors.Is(err, ErrBackpressure) {
			// The outbound limit is full: the Pong cannot be sent now and must
			// not close the connection over it. Dropping it loses only this
			// keepalive exchange; the peer's next Ping retries.
			return nil
		}
		if errors.Is(err, ErrClosed) {
			// A close in progress answers no Pings; the round goes on
			// consuming so the peer's Close still completes the handshake.
			return nil
		}
		return err
	case frame.Pong:
		if c.heartbeat != nil && len(f.Payload) == 8 {
			c.heartbeat.acknowledgePing(binary.BigEndian.Uint64(f.Payload))
		}
		return nil
	case frame.Close:
		code := frame.CloseCode(f.Payload)
		reason := ""
		if len(f.Payload) > 2 {
			reason = string(f.Payload[2:])
		}
		c.setCloseReason(code, reason)
		if err := c.sendCloseFrame(f.Payload); err != nil && !errors.Is(err, ErrClosed) {
			return err
		}
		return c.closeTransport()
	default:
		return frame.ErrProtocol
	}
}

// ownedProtocolClose marks the one error path where the Close frame has taken
// responsibility for transport shutdown. Other errors must reach UIO even if
// the connection happens to be closing concurrently.
type ownedProtocolClose struct{ cause error }

func (err *ownedProtocolClose) Error() string { return err.cause.Error() }
func (err *ownedProtocolClose) Unwrap() error { return err.cause }

func protocolCloseOwnsTransport(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*ownedProtocolClose); ok {
		return true
	}
	var owned *ownedProtocolClose
	return errors.As(err, &owned)
}

// protocolClose maps parser/resource errors to RFC close codes, attempts to
// flush the Close frame, and preserves both protocol and transport failures.
func (c *Conn) protocolClose(err error) error {
	c.setCloseError(err)
	code := uint16(1002)
	if errors.Is(err, frame.ErrInvalidUTF8) {
		code = 1007
	} else if errors.Is(err, frame.ErrMessageTooBig) || errors.Is(err, compress.ErrTooLarge) {
		code = 1009
	}
	payload := []byte{byte(code >> 8), byte(code)}
	writeErr := c.sendCloseFrame(payload)
	c.markClosing()
	if writeErr != nil {
		c.abortTransport(errors.Join(err, writeErr))
		return errors.Join(err, writeErr)
	}
	if closeErr := c.closeTransport(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		c.abortTransport(errors.Join(err, closeErr))
		return errors.Join(err, closeErr)
	}
	return &ownedProtocolClose{cause: err}
}

func messageType(op frame.OpCode) MessageType {
	if op == frame.Text {
		return TextMessage
	}
	return BinaryMessage
}
