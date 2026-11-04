package umux

import (
	"net"

	"github.com/urpc/uio/internal/readcontrol"
)

// Embed the shared private capability without adding a public field or a
// controller accessor to Stream's API.
type streamReadAccess = readcontrol.Access[streamReadControlGetter]

type streamReadControlGetter struct{}

func (streamReadControlGetter) GetReadControl(source any) *readcontrol.Control {
	return source.(interface{ streamReadControl() *readcontrol.Control }).streamReadControl()
}

func (st *Stream) streamReadControl() *readcontrol.Control { return st.ensureReadControl() }

// Stream input has its own consumer boundary. A paused stream keeps its
// input and so its receive credit, instead of pausing the shared transport or
// sibling streams. The pause controls OnRead delivery only: pull-mode reads,
// like a timer callback's reads of a paused socket, still read directly.
func (st *Stream) checkReadControl() error {
	if st.sess == nil {
		return net.ErrClosed
	}
	st.sess.mu.Lock()
	defer st.sess.mu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sess.closed || st.closedLocal {
		return net.ErrClosed
	}
	return nil
}

func (st *Stream) ensureReadControl() *readcontrol.Control {
	if control := st.readControl.Load(); control != nil {
		return control
	}
	control := readcontrol.New(st.checkReadControl, st.Wake)
	if st.readControl.CompareAndSwap(nil, control) {
		return control
	}
	return st.readControl.Load()
}

// PauseReads suppresses subsequent OnRead callbacks for this stream only.
// Timers, session control frames, writes, and other streams continue normally.
func (st *Stream) PauseReads() error {
	return st.ensureReadControl().PauseReads()
}

// ResumeReads releases the application's pause; once no pause remains, a read
// turn hands the stream's buffered input to OnRead. Other owners' pauses stay.
// Without a pause to release it does nothing.
func (st *Stream) ResumeReads() error {
	if err := st.checkReadControl(); err != nil {
		return err
	}
	if control := st.readControl.Load(); control != nil {
		return control.ResumeReads()
	}
	return nil
}

func (st *Stream) ReadsPaused() bool { return st.readControl.Load().ReadsPaused() }
