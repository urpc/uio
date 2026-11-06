package uio

import (
	"errors"

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/readcontrol"
	"github.com/urpc/uio/internal/writecontrol"
)

type blockControlGetter struct{}

type blockControlAccess = blockio.Access[blockControlGetter]

func (blockControlGetter) GetBlockState(source any) (*blockio.State, bool) {
	if boundary, ok := source.(interface{ connBlockState() (*blockio.State, bool) }); ok {
		return boundary.connBlockState()
	}
	return nil, false
}

// fdBlockSource adapts stream *fdConn (native Unix and portable stdio) to blockio.Source.
type fdBlockSource struct{ conn *fdConn }

func (s fdBlockSource) PeekChunk() []byte                 { return s.conn.PeekChunk() }
func (s fdBlockSource) Discard(n int) (int, error)        { return s.conn.Discard(n) }
func (s fdBlockSource) InboundBuffered() int              { return s.conn.InboundBuffered() }
func (s fdBlockSource) ReadEnded() bool                   { return s.conn.State()&StateReadEnded != 0 }
func (s fdBlockSource) WriteEnded() bool                  { return s.conn.writeSideClosed() }
func (s fdBlockSource) ClosingOrClosed() bool             { return s.conn.isClosing() }
func (s fdBlockSource) Write(p []byte) (int, error)       { return s.conn.writeNonBlocking(p) }
func (s fdBlockSource) Writev(v [][]byte) (int, error)    { return s.conn.writevNonBlocking(v) }
func (s fdBlockSource) WriteOwned(b *Buffer) (int, error) { return s.conn.writeOwnedNonBlocking(b) }
func (s fdBlockSource) Flush() error                      { return s.conn.Flush() }
func (s fdBlockSource) OutboundBuffered() int             { return s.conn.OutboundBuffered() }
func (s fdBlockSource) MaxInbound() int                   { return s.conn.events().MaxInboundBuffered }
func (s fdBlockSource) MaxOutbound() int                  { return s.conn.events().effectiveMaxOutbound() }
func (s fdBlockSource) EnablePullRead(onTurn bool) error  { return s.conn.enablePullRead(onTurn) }
func (s fdBlockSource) Wake() error                       { return s.conn.Wake() }
func (s fdBlockSource) NewReadPause() *readcontrol.Pause  { return readcontrol.NewPause(s.conn) }
func (s fdBlockSource) WatchWrite(fn func(int)) bool      { return writecontrol.Watch(s.conn, fn) }
func (s fdBlockSource) IsOverflow(err error) bool         { return errors.Is(err, ErrOutboundOverflow) }

// stackBlockSource adapts the top *layerNode of a protocol stack to blockio.Source.
type stackBlockSource struct{ top *layerNode }

func (s stackBlockSource) PeekChunk() []byte                 { return s.top.PeekChunk() }
func (s stackBlockSource) Discard(n int) (int, error)        { return s.top.Discard(n) }
func (s stackBlockSource) InboundBuffered() int              { return s.top.InboundBuffered() }
func (s stackBlockSource) ReadEnded() bool                   { return s.top.State()&StateReadEnded != 0 }
func (s stackBlockSource) WriteEnded() bool                  { return s.top.gate.Load()&gateWriteShut != 0 }
func (s stackBlockSource) ClosingOrClosed() bool             { return s.top.stack.over() }
func (s stackBlockSource) Write(p []byte) (int, error)       { return s.top.writeNonBlocking(p) }
func (s stackBlockSource) Writev(v [][]byte) (int, error)    { return s.top.writevNonBlocking(v) }
func (s stackBlockSource) WriteOwned(b *Buffer) (int, error) { return s.top.writeOwnedNonBlocking(b) }
func (s stackBlockSource) Flush() error                      { return s.top.Flush() }
func (s stackBlockSource) OutboundBuffered() int             { return s.top.OutboundBuffered() }
func (s stackBlockSource) MaxInbound() int                   { return s.top.stack.inLimit }
func (s stackBlockSource) MaxOutbound() int                  { return s.top.stack.limit }
func (s stackBlockSource) Wake() error                       { return s.top.Wake() }
func (s stackBlockSource) NewReadPause() *readcontrol.Pause  { return readcontrol.NewPause(s.top) }
func (s stackBlockSource) WatchWrite(fn func(int)) bool      { return writecontrol.Watch(s.top, fn) }
func (s stackBlockSource) IsOverflow(err error) bool         { return errors.Is(err, ErrOutboundOverflow) }

func (s stackBlockSource) EnablePullRead(onTurn bool) error {
	s.top.stack.pullRead.Store(true)
	if !onTurn && !s.top.stack.over() {
		s.top.stack.requestTurn()
	}
	return nil
}

func (l *layerNode) connBlockState() (*blockio.State, bool) {
	if l == nil || !l.isTop() {
		return nil, false
	}
	c := l.stack
	if st := c.block.Load(); st != nil {
		return st, true
	}
	st := blockio.NewState(stackBlockSource{top: l})
	if c.block.CompareAndSwap(nil, st) {
		return st, true
	}
	return c.block.Load(), true
}

func (l *layerNode) hasWaitingBlockWriter() bool {
	return l.stack.block.Load().HasWaitingWriter()
}

func (l *layerNode) ensureBlockWriter() *blockio.Writer {
	st, _ := l.connBlockState()
	return st.Writer(0)
}
