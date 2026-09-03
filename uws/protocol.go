package uws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/extension"
	"github.com/urpc/uio/uws/internal/frame"
	"github.com/urpc/uio/uws/internal/handshake"
)

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
	if state.timer != nil {
		state.timer.Stop()
	}
	state.expired = false
	state.timer = time.AfterFunc(timeout, func() {
		c.expireHandshake(state, epoch, context.DeadlineExceeded)
	})
	state.mu.Unlock()
	if stopContext != nil {
		stopContext()
	}
}

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
	state.epoch++
	timer := state.timer
	state.timer = nil
	stopContext := state.contextStop
	state.contextStop = nil
	cleanup := state.cleanup
	state.cleanup = nil
	state.mu.Unlock()
	c.handshake.CompareAndSwap(state, nil)
	if timer != nil {
		timer.Stop()
	}
	if stopContext != nil {
		stopContext()
	}
	if cleanup != nil {
		cleanup()
	}
	_ = c.raw.CloseWith(cause)
}

func (c *Conn) stopHandshakeTimer() {
	state := c.handshake.Load()
	if state == nil {
		return
	}
	state.mu.Lock()
	state.epoch++
	timer := state.timer
	state.timer = nil
	stopContext := state.contextStop
	state.contextStop = nil
	cleanup := state.cleanup
	state.cleanup = nil
	state.mu.Unlock()
	c.handshake.CompareAndSwap(state, nil)
	if timer != nil {
		timer.Stop()
	}
	if stopContext != nil {
		stopContext()
	}
	if cleanup != nil {
		cleanup()
	}
}

func (c *Conn) markOpened() bool {
	state := c.handshake.Load()
	if state == nil {
		return false
	}
	state.mu.Lock()
	if state.expired || c.closed.Load() {
		state.mu.Unlock()
		return false
	}
	state.epoch++
	timer := state.timer
	state.timer = nil
	stopContext := state.contextStop
	state.contextStop = nil
	cleanup := state.cleanup
	state.cleanup = nil
	c.opened.Store(true)
	state.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if stopContext != nil {
		stopContext()
	}
	if cleanup != nil {
		cleanup()
	}
	return true
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
	state.data = nil
	state.upgrade = nil
	state.clientKey = ""
	c.handshake.CompareAndSwap(state, nil)
}

func (c *Conn) readAvailable() error {
	frames := 0
	for c.raw.InboundBuffered() > 0 {
		data := c.raw.PeekChunk()
		if len(data) == 0 {
			return nil
		}
		if !c.opened.Load() {
			err := c.consumeHandshake(data)
			if err != nil {
				_, _ = c.raw.Discard(len(data))
				c.rejectHandshake()
				return err
			}
			_, _ = c.raw.Discard(len(data))
			continue
		}

		consumed, err := c.feedFrames(data, func(f frame.Frame) error {
			if err := c.acceptFrame(f); err != nil {
				return err
			}
			frames++
			if isReadPaused(c.raw) {
				return errReadPaused
			}
			if frames >= maxFramesPerDataEvent {
				return errReadBudget
			}
			return nil
		})
		if consumed > 0 {
			_, _ = c.raw.Discard(consumed)
		}
		if errors.Is(err, errReadPaused) {
			return nil
		}
		if errors.Is(err, errReadBudget) {
			if c.raw.InboundBuffered() == 0 {
				return nil
			}
			if wakeErr := c.raw.Wake(); wakeErr != nil {
				if c.closing.Load() || c.closed.Load() {
					return nil
				}
				return fmt.Errorf("uws: schedule buffered read: %w", wakeErr)
			}
			return nil
		}
		if err != nil {
			return c.protocolClose(err)
		}
		if consumed == 0 {
			return nil
		}
	}
	return nil
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

func (c *Conn) consumeHandshake(data []byte) error {
	state := c.handshake.Load()
	if state == nil {
		return ErrClosed
	}
	state.data = append(state.data, data...)
	if c.isClient() {
		err := c.consumeClientHandshake()
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return err
	}
	req, consumed, err := handshake.ParseServerRequest(state.data, handshake.ServerOptions{
		MaxHeaderBytes: c.config.maxHeaderBytes,
		CheckOrigin:    c.config.checkOrigin,
	})
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return err
	}
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
		c.heartbeat.lastPong.Store(time.Now().UnixNano())
		c.config.heartbeatConnections.Store(c, c)
	}
	extra := append([]byte(nil), state.data[consumed:]...)
	c.releaseHandshakeState(state)
	if err := c.dispatchOpen(); err != nil {
		return err
	}
	if len(extra) > 0 {
		if _, err := c.feedFrames(extra, c.acceptFrame); err != nil {
			return c.protocolClose(err)
		}
	}
	return nil
}

func (c *Conn) consumeClientHandshake() error {
	state := c.handshake.Load()
	if state == nil {
		return ErrClosed
	}
	if len(state.data) > c.config.maxHeaderBytes {
		return handshake.ErrBadRequest
	}
	consumed, protocol, extensions, err := handshake.ValidateClientResponse(state.data, state.clientKey, c.config.subprotocols, c.config.maxHeaderBytes)
	if err != nil {
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
	extra := append([]byte(nil), state.data[consumed:]...)
	c.releaseHandshakeState(state)
	if err := c.dispatchOpen(); err != nil {
		return err
	}
	if len(extra) > 0 {
		if _, err := c.feedFrames(extra, c.acceptFrame); err != nil {
			return c.protocolClose(err)
		}
	}
	return nil
}

func (c *Conn) rejectHandshake() {
	if c.opened.Load() || c.closed.Load() {
		return
	}
	if c.isClient() {
		_ = c.raw.Close()
		return
	}
	const response = "HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"
	_, _ = c.raw.Write([]byte(response))
}

func (c *Conn) closeTransport() error {
	if err := c.flush(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	c.transportClosePending.Store(true)
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

func (c *Conn) startCloseTimer() {
	if c.closed.Load() {
		return
	}
	state := c.ensureCloseTimerState()
	state.mu.Lock()
	if c.closed.Load() {
		state.mu.Unlock()
		return
	}
	if state.timer != nil {
		state.timer.Stop()
	}
	state.timer = time.AfterFunc(c.closeTimeout(), func() {
		_ = c.raw.CloseWith(io.ErrClosedPipe)
	})
	state.mu.Unlock()
}

func (c *Conn) ensureCloseTimer() {
	if c.closed.Load() {
		return
	}
	state := c.ensureCloseTimerState()
	state.mu.Lock()
	if c.closed.Load() {
		state.mu.Unlock()
		return
	}
	if state.timer == nil {
		state.timer = time.AfterFunc(c.closeTimeout(), func() {
			_ = c.raw.CloseWith(io.ErrClosedPipe)
		})
	}
	state.mu.Unlock()
}

func (c *Conn) tryCloseTransport() (bool, error) {
	if !c.transportClosePending.Load() || c.pendingBytes.Load() != 0 {
		return false, nil
	}
	if !c.transportClosePending.CompareAndSwap(true, false) {
		return false, nil
	}
	c.stopCloseTimer()
	return true, c.raw.CloseWith(io.EOF)
}

func (c *Conn) stopCloseTimer() {
	state := c.closeTimer.Load()
	if state == nil {
		return
	}
	state.mu.Lock()
	timer := state.timer
	state.timer = nil
	state.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
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

func (c *Conn) acceptFrame(f frame.Frame) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if c.assembler == nil && (frame.IsControl(f.Opcode) || ((f.Opcode == frame.Text || f.Opcode == frame.Binary) && f.Fin)) {
		return frame.AcceptSingle(f, &c.config.assembler, c.acceptControl, c.acceptMessage)
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

func (c *Conn) acceptMessage(message frame.Message) error {
	if message.Compressed {
		if c.compression == nil || c.compression.decoder == nil {
			return frame.ErrProtocol
		}
		return c.compression.decoder.DecodeBorrowed(message.Payload, c.maxMessageSizeInt(), func(payload []byte) error {
			if message.Opcode == frame.Text && c.utf8ValidationEnabled() && !utf8.Valid(payload) {
				return frame.ErrInvalidUTF8
			}
			return c.enqueueMessage(Message{Type: messageType(message.Opcode), Payload: payload})
		})
	}
	return c.enqueueMessage(Message{Type: messageType(message.Opcode), Payload: message.Payload})
}

func (c *Conn) acceptControl(f frame.Frame) error {
	switch f.Opcode {
	case frame.Ping:
		return c.sendFrame(frame.Frame{Fin: true, Opcode: frame.Pong, Payload: f.Payload})
	case frame.Pong:
		if c.heartbeat != nil {
			c.heartbeat.lastPong.Store(time.Now().UnixNano())
			c.heartbeat.pingOutstanding.Store(false)
		}
		return nil
	case frame.Close:
		code := frame.CloseCode(f.Payload)
		reason := ""
		if len(f.Payload) > 2 {
			reason = string(f.Payload[2:])
		}
		c.setCloseReason(code, reason)
		if err := c.sendFrame(frame.Frame{Fin: true, Opcode: frame.Close, Payload: f.Payload}); err != nil && !errors.Is(err, ErrClosed) {
			return err
		}
		c.closing.Store(true)
		return c.closeTransport()
	default:
		return frame.ErrProtocol
	}
}

func (c *Conn) protocolClose(err error) error {
	c.setCloseError(err)
	code := uint16(1002)
	if errors.Is(err, frame.ErrInvalidUTF8) {
		code = 1007
	} else if errors.Is(err, frame.ErrMessageTooBig) || errors.Is(err, compress.ErrTooLarge) {
		code = 1009
	} else if errors.Is(err, ErrApplicationBackpressure) {
		code = 1013
	}
	payload := []byte{byte(code >> 8), byte(code)}
	writeErr := c.sendFrame(frame.Frame{Fin: true, Opcode: frame.Close, Payload: payload})
	c.closing.Store(true)
	if writeErr != nil {
		_ = c.raw.CloseWith(errors.Join(err, writeErr))
		return errors.Join(err, writeErr)
	}
	if closeErr := c.closeTransport(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		_ = c.raw.CloseWith(errors.Join(err, closeErr))
		return errors.Join(err, closeErr)
	}
	return err
}

func messageType(op frame.OpCode) MessageType {
	if op == frame.Text {
		return TextMessage
	}
	return BinaryMessage
}
