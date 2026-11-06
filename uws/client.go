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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/petermattis/goid"
	"github.com/urpc/uio"
	"github.com/urpc/uio/internal/blockio"
	_ "github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/writecontrol"
)

// DefaultDialer is the Dialer the package-level Dial uses. It runs with the
// default options and lives until the process ends, like http.DefaultClient;
// close it and every connection it made by calling DefaultDialer.Close.
var DefaultDialer = &Dialer{}

const (
	// DefaultBlockingQueueMessages bounds queued inbound messages before reads
	// pause on the underlying transport.
	DefaultBlockingQueueMessages = 64
	// DefaultBlockingQueueBytes is the default byte budget (64 KiB) for
	// inbound message queuing and soft outbound write backpressure.
	DefaultBlockingQueueBytes = uio.DefaultBlockingBuffer

	clientQueueMessages = DefaultBlockingQueueMessages
)

// ErrBlockingOnTurn reports an attempt to invoke a blocking operation on a
// connection turn or event-loop goroutine.
var ErrBlockingOnTurn = uio.ErrBlockingOnTurn

type blockingOptions struct {
	readBytes  int
	readMsgs   int
	writeBytes int
}

// BlockingOption configures a BlockingConn (or Client).
type BlockingOption interface {
	applyBlocking(*blockingOptions)
}

type blockingOptionFunc func(*blockingOptions)

func (f blockingOptionFunc) applyBlocking(o *blockingOptions) { f(o) }

// WithReadQueueBytes sets the inbound byte watermark at which BlockingConn
// pauses reading from the underlying transport.
func WithReadQueueBytes(bytes int) BlockingOption {
	return blockingOptionFunc(func(o *blockingOptions) { o.readBytes = bytes })
}

// WithReadQueueMessages sets the maximum number of queued inbound messages
// before BlockingConn pauses reading from the underlying transport.
func WithReadQueueMessages(count int) BlockingOption {
	return blockingOptionFunc(func(o *blockingOptions) { o.readMsgs = count })
}

// WithWriteBuffer sets the soft outbound byte window for BlockingConn's
// blocking WriteMessage / WriteText / WriteBinary methods.
func WithWriteBuffer(bytes int) BlockingOption {
	return blockingOptionFunc(func(o *blockingOptions) { o.writeBytes = bytes })
}

func resolveBlockingOpts(conn *Conn, opts []BlockingOption) blockingOptions {
	res := blockingOptions{
		readBytes:  DefaultBlockingQueueBytes,
		readMsgs:   DefaultBlockingQueueMessages,
		writeBytes: DefaultBlockingQueueBytes,
	}
	if conn != nil && conn.config != nil {
		if conn.config.maxInbound > 0 {
			res.readBytes = conn.config.maxInbound
		}
		if conn.config.maxOutbound > 0 {
			res.writeBytes = conn.config.maxOutbound
		}
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyBlocking(&res)
		}
	}
	if res.readBytes <= 0 {
		res.readBytes = DefaultBlockingQueueBytes
	}
	if res.readMsgs <= 0 {
		res.readMsgs = DefaultBlockingQueueMessages
	}
	if res.writeBytes <= 0 {
		res.writeBytes = DefaultBlockingQueueBytes
	}
	return res
}

type queuedMessage struct {
	typ MessageType
	buf *uio.Buffer
}

type msgRing struct {
	items []queuedMessage
	head  int
	count int
	bytes int
}

func (r *msgRing) push(typ MessageType, payload []byte) {
	if r.count == len(r.items) {
		nextCap := max(16, len(r.items)*2)
		next := make([]queuedMessage, nextCap)
		for i := range r.count {
			next[i] = r.items[(r.head+i)%len(r.items)]
		}
		r.items = next
		r.head = 0
	}
	var b *uio.Buffer
	if len(payload) > 0 {
		b = uio.AcquireBuffer(len(payload))
		dst := b.AvailableBuffer()[:len(payload)]
		copy(dst, payload)
		b.CommitWrite(len(payload))
	}
	idx := (r.head + r.count) % len(r.items)
	r.items[idx] = queuedMessage{typ: typ, buf: b}
	r.count++
	r.bytes += len(payload)
}

func (r *msgRing) pop() (queuedMessage, int) {
	item := r.items[r.head]
	r.items[r.head] = queuedMessage{}
	r.head = (r.head + 1) % len(r.items)
	r.count--
	if r.count == 0 {
		r.head = 0
	}
	n := 0
	if item.buf != nil {
		n = item.buf.Len()
	}
	r.bytes -= n
	return item, n
}

type parkedMsgReader struct {
	into   bool
	dst    []byte
	typ    MessageType
	out    []byte
	filled bool
}

// BlockingConn is a synchronous, message-oriented WebSocket endpoint usable on
// both client connections (via Dial / Dialer.DialClient) and upgraded server
// connections (via NewBlockingConn in OnOpen or OnMessage).
//
// Inbound backpressure pauses the underlying transport via Conn.PauseRead
// without ever blocking connection turn workers, so heartbeats, transport
// deadlines, and shutdowns continue to run natively on the event loop.
type BlockingConn struct {
	*Conn

	rmu     sync.Mutex // serializes concurrent ReadMessage / ReadMessageInto callers
	wmu     sync.Mutex // serializes concurrent WriteMessage / WriteText / WriteBinary / Flush callers
	pauseMu sync.Mutex // serializes Conn.PauseRead / ResumeRead transitions
	mu      sync.Mutex // guards ring, parked reader, pause flag, and intake state

	ring         msgRing
	parkedReader *parkedMsgReader
	maxReadBytes int
	maxReadMsgs  int
	maxWrite     int
	paused       bool
	intakeDone   bool

	inGen  atomic.Uint64
	inPark atomic.Bool
	inSig  chan struct{}

	outGen         atomic.Uint64
	outPark        atomic.Bool
	outSig         chan struct{}
	writeWatchOnce sync.Once

	ended     chan struct{}
	endedOnce sync.Once
	event     atomic.Pointer[CloseEvent]
}

// Client is a type alias for BlockingConn, preserving full backward
// compatibility for Dial and Dialer.DialClient callers.
type Client = BlockingConn

// Dial connects with DefaultDialer and returns the synchronous client once the
// WebSocket handshake completes.
func Dial(ctx context.Context, addr string, opts ...BlockingOption) (*Client, error) {
	return DefaultDialer.DialClient(ctx, addr, opts...)
}

func newBlockingConn(conn *Conn, cfg blockingOptions) *BlockingConn {
	return &BlockingConn{
		Conn:         conn,
		maxReadBytes: cfg.readBytes,
		maxReadMsgs:  cfg.readMsgs,
		maxWrite:     cfg.writeBytes,
		inSig:        make(chan struct{}, 1),
		outSig:       make(chan struct{}, 1),
		ended:        make(chan struct{}),
	}
}

// NewBlockingConn switches an open *Conn (such as a server connection inside
// OnOpen or OnMessage) from push OnMessage callbacks to synchronous pull/push
// BlockingConn operations. Calling NewBlockingConn again on the same *Conn
// returns the existing *BlockingConn.
//
// When upgrading an already-open push *Conn, call NewBlockingConn inside OnOpen
// or OnMessage so the handler handoff is serialized with frame delivery.
func NewBlockingConn(conn *Conn, opts ...BlockingOption) (*BlockingConn, error) {
	if conn == nil || conn.raw == nil || !conn.opened.Load() {
		return nil, ErrNotReady
	}
	if conn.closed.Load() {
		return nil, ErrClosed
	}
	meta := conn.ensureMetadata()
	if bc := meta.blocking.Load(); bc != nil {
		return bc, nil
	}
	onOwn := conn.turnGoid.Load() == goid.Get()
	if !onOwn {
		if ot, ok := conn.raw.(interface{ InOwnTurn() bool }); ok {
			onOwn = ot.InOwnTurn()
		} else {
			onOwn = blockio.OnTurn()
		}
	}
	if !onOwn {
		return nil, fmt.Errorf("%w: NewBlockingConn on an active connection must run in its own OnOpen or OnMessage", ErrInvalidOptions)
	}
	cfg := resolveBlockingOpts(conn, opts)
	bc := newBlockingConn(conn, cfg)
	if !meta.blocking.CompareAndSwap(nil, bc) {
		return meta.blocking.Load(), nil
	}
	conn.hasBlocking = true
	if conn.externalReadPaused() {
		_ = conn.ResumeRead()
	}
	return bc, nil
}

// InboundMessages reports the number of unread messages currently queued.
func (c *BlockingConn) InboundMessages() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ring.count
}

// InboundBytes reports the total payload bytes of unread messages currently queued.
func (c *BlockingConn) InboundBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ring.bytes
}

func (c *BlockingConn) notifyIn() {
	c.inGen.Add(1)
	if c.inPark.Swap(false) {
		select {
		case c.inSig <- struct{}{}:
		default:
		}
	}
}

func (c *BlockingConn) notifyWriteSpace() {
	c.outGen.Add(1)
	if c.outPark.Swap(false) {
		select {
		case c.outSig <- struct{}{}:
		default:
		}
	}
}

func (c *BlockingConn) syncReadPause(conn *Conn) {
	if conn == nil {
		return
	}
	c.pauseMu.Lock()
	defer c.pauseMu.Unlock()
	c.mu.Lock()
	wantPaused := c.paused && !c.intakeDone
	c.mu.Unlock()
	if wantPaused {
		_ = conn.PauseRead()
	} else if conn.externalReadPaused() {
		_ = conn.ResumeRead()
	}
}

func (c *BlockingConn) enqueue(conn *Conn, msg Message) {
	c.mu.Lock()
	if c.intakeDone {
		c.mu.Unlock()
		return
	}
	if slot := c.parkedReader; slot != nil {
		slot.typ = msg.Type
		if slot.into {
			slot.out = append(slot.dst[:0], msg.Payload...)
		} else if len(msg.Payload) > 0 {
			slot.out = append([]byte(nil), msg.Payload...)
		} else {
			slot.out = nil
		}
		slot.filled = true
		c.parkedReader = nil
		c.mu.Unlock()
		c.notifyIn()
		return
	}

	c.ring.push(msg.Type, msg.Payload)
	needPause := false
	if (c.ring.count >= c.maxReadMsgs || c.ring.bytes >= c.maxReadBytes) && !c.paused {
		c.paused = true
		needPause = true
	}
	c.mu.Unlock()

	if needPause {
		c.syncReadPause(conn)
	}
	c.notifyIn()
}

func (c *BlockingConn) checkResumeLocked() bool {
	if c.paused && c.ring.count <= c.maxReadMsgs/2 && c.ring.bytes <= c.maxReadBytes/2 {
		c.paused = false
		return true
	}
	return false
}

// CloseError reports how a BlockingConn's connection ended. ReadMessage and
// ReadMessageInto return it after the connection closed and every queued
// message was returned.
type CloseError struct {
	CloseEvent
}

func (e *CloseError) Error() string {
	if e.Err != nil {
		return "uws: connection closed: " + e.Err.Error()
	}
	return "uws: connection closed"
}

func (e *CloseError) Unwrap() error { return e.Err }

// ReadMessage returns the next received WebSocket message, blocking until one
// arrives, the connection closes, or ctx ends. A canceled or timed-out ctx
// returns ctx.Err() without closing the connection.
func (c *BlockingConn) ReadMessage(ctx context.Context) (Message, error) {
	typ, payload, err := c.readNext(ctx, false, nil)
	if err != nil {
		return Message{}, err
	}
	return Message{Type: typ, Payload: payload}, nil
}

// ReadMessageInto appends the next received message's payload into dst[:0]
// (reusing dst's backing array when large enough for zero heap allocations)
// and returns the message type and populated slice.
func (c *BlockingConn) ReadMessageInto(ctx context.Context, dst []byte) (MessageType, []byte, error) {
	return c.readNext(ctx, true, dst)
}

func (c *BlockingConn) readNext(ctx context.Context, into bool, dst []byte) (MessageType, []byte, error) {
	if blockio.OnTurn() {
		return 0, nil, ErrBlockingOnTurn
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}

	c.rmu.Lock()
	defer c.rmu.Unlock()

	c.mu.Lock()
	var slot parkedMsgReader
	for {
		tok := c.inGen.Load()
		c.inPark.Store(true)

		if c.ring.count > 0 {
			c.parkedReader = nil
			c.inPark.Store(false)
			item, _ := c.ring.pop()
			resume := c.checkResumeLocked()
			c.mu.Unlock()
			if resume {
				c.syncReadPause(c.Conn)
			}
			typ, out := consumeQueuedItem(item, into, dst)
			return typ, out, nil
		}
		if c.intakeDone {
			c.parkedReader = nil
			c.inPark.Store(false)
			c.mu.Unlock()
			return 0, nil, c.awaitCloseError(ctx)
		}
		if err := ctx.Err(); err != nil {
			c.parkedReader = nil
			c.inPark.Store(false)
			c.mu.Unlock()
			return 0, nil, err
		}

		slot = parkedMsgReader{into: into, dst: dst}
		c.parkedReader = &slot
		c.mu.Unlock()

		if c.inGen.Load() == tok {
			select {
			case <-c.inSig:
			case <-c.ended:
			case <-ctx.Done():
			}
		} else {
			c.inPark.Store(false)
		}

		c.mu.Lock()
		if slot.filled {
			c.mu.Unlock()
			return slot.typ, slot.out, nil
		}
		c.parkedReader = nil
	}
}

func consumeQueuedItem(item queuedMessage, into bool, dst []byte) (MessageType, []byte) {
	if item.buf == nil {
		if into {
			return item.typ, dst[:0]
		}
		return item.typ, nil
	}
	src := item.buf.Bytes()
	var out []byte
	if into {
		out = append(dst[:0], src...)
	} else {
		out = append([]byte(nil), src...)
	}
	uio.ReleaseBuffer(item.buf)
	return item.typ, out
}

func (c *BlockingConn) awaitCloseError(ctx context.Context) error {
	if ev := c.event.Load(); ev != nil {
		return &CloseError{CloseEvent: *ev}
	}
	select {
	case <-c.ended:
		if ev := c.event.Load(); ev != nil {
			return &CloseError{CloseEvent: *ev}
		}
		return &CloseError{CloseEvent: CloseEvent{Err: ErrClosed}}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WriteText sends a text WebSocket message, suspending when the outbound
// buffer is full until space drains, the connection closes, or ctx ends.
func (c *BlockingConn) WriteText(ctx context.Context, text string) error {
	if len(text) == 0 {
		return c.WriteMessage(ctx, TextMessage, nil)
	}
	data := unsafe.Slice(unsafe.StringData(text), len(text))
	return c.WriteMessage(ctx, TextMessage, data)
}

// WriteBinary sends a binary WebSocket message, suspending when the outbound
// buffer is full until space drains, the connection closes, or ctx ends.
func (c *BlockingConn) WriteBinary(ctx context.Context, payload []byte) error {
	return c.WriteMessage(ctx, BinaryMessage, payload)
}

// WriteMessage sends one message of typ, suspending on outbound backpressure
// until space drains, the connection closes, or ctx ends.
func (c *BlockingConn) WriteMessage(ctx context.Context, typ MessageType, payload []byte) error {
	if blockio.OnTurn() {
		return ErrBlockingOnTurn
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if c.Conn == nil {
		return ErrNotReady
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()

	if maxOut := c.Conn.maxOutbound(); maxOut > 0 && c.Conn.compression == nil {
		if fragmentedWireSize(len(payload), c.Conn.maxFramePayload(), c.Conn.isClient()) > maxOut {
			return ErrBackpressure
		}
	}

	c.ensureWriteWatch()
	limit := c.maxWrite
	for {
		tok := c.outGen.Load()
		c.outPark.Store(true)

		if err := ctx.Err(); err != nil {
			c.outPark.Store(false)
			return err
		}
		if c.Conn.closed.Load() || c.Conn.closing.Load() {
			c.outPark.Store(false)
			if ev := c.event.Load(); ev != nil {
				return &CloseError{CloseEvent: *ev}
			}
			return ErrClosed
		}
		buffered := c.Conn.OutboundBuffered()
		if buffered == 0 || buffered < limit {
			wasEmpty := buffered == 0
			err := c.Conn.send(typ, payload)
			if err == nil {
				c.outPark.Store(false)
				return nil
			}
			if !errors.Is(err, ErrBackpressure) {
				c.outPark.Store(false)
				return err
			}
			if wasEmpty && c.Conn.OutboundBuffered() == 0 && c.outGen.Load() == tok {
				c.outPark.Store(false)
				return err
			}
		} else {
			_ = c.Conn.flushAll()
		}
		if c.outGen.Load() != tok {
			c.outPark.Store(false)
			continue
		}
		select {
		case <-c.outSig:
		case <-c.ended:
		case <-ctx.Done():
			c.outPark.Store(false)
			return ctx.Err()
		}
	}
}

// Flush flushes queued outbound frames and blocks until the outbound buffer
// drains, the connection closes, or ctx ends.
func (c *BlockingConn) Flush(ctx context.Context) error {
	if blockio.OnTurn() {
		return ErrBlockingOnTurn
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if c.Conn == nil {
		return ErrNotReady
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()

	if err := c.Conn.flushAll(); err != nil {
		return err
	}
	if c.Conn.OutboundBuffered() == 0 {
		return nil
	}
	c.ensureWriteWatch()
	for {
		tok := c.outGen.Load()
		c.outPark.Store(true)
		if err := ctx.Err(); err != nil {
			c.outPark.Store(false)
			return err
		}
		if c.Conn.closed.Load() {
			c.outPark.Store(false)
			if ev := c.event.Load(); ev != nil {
				return &CloseError{CloseEvent: *ev}
			}
			return ErrClosed
		}
		if c.Conn.OutboundBuffered() == 0 {
			c.outPark.Store(false)
			return nil
		}
		if c.outGen.Load() != tok {
			c.outPark.Store(false)
			continue
		}
		select {
		case <-c.outSig:
		case <-c.ended:
		case <-ctx.Done():
			c.outPark.Store(false)
			return ctx.Err()
		}
	}
}

func (c *BlockingConn) ensureWriteWatch() {
	c.writeWatchOnce.Do(func() {
		if c.Conn != nil && c.Conn.raw != nil {
			writecontrol.Watch(c.Conn.raw, func(int) {
				c.notifyWriteSpace()
			})
		}
	})
}

// CloseEventInfo reports how the connection ended once closed.
func (c *BlockingConn) CloseEventInfo() CloseEvent {
	if event := c.event.Load(); event != nil {
		return *event
	}
	return CloseEvent{}
}

// Close validates code and reason, stops further inbound message intake (while
// preserving already-queued messages for subsequent ReadMessage calls), and
// initiates the WebSocket Close handshake.
func (c *BlockingConn) Close(code uint16, reason string) error {
	if c.Conn == nil {
		return ErrNotReady
	}
	if _, err := c.Conn.closePayload(code, reason); err != nil {
		return err
	}
	c.mu.Lock()
	c.intakeDone = true
	c.paused = false
	c.mu.Unlock()
	c.notifyIn()
	c.notifyWriteSpace()
	return c.Conn.Close(code, reason)
}

func (c *BlockingConn) onConnClose(event CloseEvent) {
	c.event.Store(&event)
	c.mu.Lock()
	c.intakeDone = true
	c.paused = false
	c.mu.Unlock()
	c.endedOnce.Do(func() { close(c.ended) })
	c.notifyIn()
	c.notifyWriteSpace()
}

type blockingHandler struct {
	bc    *BlockingConn
	ready chan error
	opts  []BlockingOption
}

func (h *blockingHandler) OnOpen(conn *Conn) {
	cfg := resolveBlockingOpts(conn, h.opts)
	h.bc.Conn = conn
	h.bc.maxReadBytes = cfg.readBytes
	h.bc.maxReadMsgs = cfg.readMsgs
	h.bc.maxWrite = cfg.writeBytes
	conn.ensureMetadata().blocking.Store(h.bc)
	conn.hasBlocking = true
	if h.ready != nil {
		h.ready <- nil
	}
}

func (h *blockingHandler) OnMessage(conn *Conn, message Message) {
	h.bc.enqueue(conn, message)
}

func (h *blockingHandler) OnClose(_ *Conn, event CloseEvent) {
	h.bc.onConnClose(event)
	if h.ready != nil {
		select {
		case h.ready <- &CloseError{CloseEvent: event}:
		default:
		}
	}
}

// DialClient connects and returns the synchronous Client (*BlockingConn) once
// the WebSocket handshake completes. Options.Handler must be nil.
func (d *Dialer) DialClient(ctx context.Context, addr string, opts ...BlockingOption) (*Client, error) {
	if blockio.OnTurn() {
		return nil, ErrBlockingOnTurn
	}
	if d.Options.Handler != nil {
		return nil, fmt.Errorf("%w: Handler is set: DialClient owns the callbacks", ErrInvalidOptions)
	}
	bc := newBlockingConn(nil, blockingOptions{
		readBytes:  DefaultBlockingQueueBytes,
		readMsgs:   DefaultBlockingQueueMessages,
		writeBytes: DefaultBlockingQueueBytes,
	})
	handler := &blockingHandler{
		bc:    bc,
		ready: make(chan error, 1),
		opts:  opts,
	}
	if err := d.dialWithHandler(ctx, addr, nil, handler); err != nil {
		return nil, err
	}
	if err := <-handler.ready; err != nil {
		return nil, err
	}
	return bc, nil
}
