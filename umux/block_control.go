package umux

import (
	"errors"

	"github.com/urpc/uio"
	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/readcontrol"
	"github.com/urpc/uio/internal/writecontrol"
)

type streamBlockAccess = blockio.Access[streamBlockControlGetter]

type streamBlockControlGetter struct{}

func (streamBlockControlGetter) GetBlockState(source any) (*blockio.State, bool) {
	return source.(interface{ streamBlockState() (*blockio.State, bool) }).streamBlockState()
}

type streamBlockSource struct{ st *Stream }

func (s streamBlockSource) PeekChunk() []byte          { return s.st.PeekChunk() }
func (s streamBlockSource) Discard(n int) (int, error) { return s.st.Discard(n) }
func (s streamBlockSource) InboundBuffered() int       { return s.st.InboundBuffered() }
func (s streamBlockSource) ReadEnded() bool            { return s.st.State()&uio.StateReadEnded != 0 }
func (s streamBlockSource) WriteEnded() bool           { return s.st.State()&uio.StateWriteEnded != 0 }
func (s streamBlockSource) ClosingOrClosed() bool {
	return s.st.State()&(uio.StateClosing|uio.StateClosed) != 0
}
func (s streamBlockSource) Write(p []byte) (int, error)           { return s.st.Write(p) }
func (s streamBlockSource) WriteOwned(b *uio.Buffer) (int, error) { return s.st.WriteOwned(b) }
func (s streamBlockSource) Flush() error                          { return s.st.Flush() }
func (s streamBlockSource) OutboundBuffered() int                 { return s.st.OutboundBuffered() }
func (s streamBlockSource) MaxInbound() int                       { return s.st.sess.cfg.WindowSize }
func (s streamBlockSource) MaxOutbound() int                      { return s.st.sess.cfg.WindowSize }
func (s streamBlockSource) Wake() error                           { return s.st.Wake() }
func (s streamBlockSource) NewReadPause() *readcontrol.Pause      { return readcontrol.NewPause(s.st) }
func (s streamBlockSource) WatchWrite(fn func(int)) bool          { return writecontrol.Watch(s.st, fn) }
func (s streamBlockSource) IsOverflow(err error) bool             { return errors.Is(err, uio.ErrOutboundOverflow) }

func (s streamBlockSource) EnablePullRead(onTurn bool) error {
	s.st.pullRead.Store(true)
	if !onTurn {
		_ = s.st.Wake()
	}
	return nil
}

func (st *Stream) streamBlockState() (*blockio.State, bool) {
	if st == nil {
		return nil, false
	}
	if b := st.blockControl.Load(); b != nil {
		return b, true
	}
	b := blockio.NewState(streamBlockSource{st: st})
	if st.blockControl.CompareAndSwap(nil, b) {
		return b, true
	}
	return st.blockControl.Load(), true
}

func (st *Stream) activeBlockReader() *blockio.Reader {
	if !st.pullRead.Load() {
		return nil
	}
	if b := st.blockControl.Load(); b != nil {
		return b.ActiveReader()
	}
	return nil
}

func (st *Stream) notifyBlockClose(err error) {
	if b := st.blockControl.Load(); b != nil {
		b.OnClose(st.localClosed.Load(), err)
	}
}
