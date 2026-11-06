package umux

import "github.com/urpc/uio/internal/writecontrol"

// streamWriteAccess embeds the shared private write-control capability,
// symmetric with streamReadAccess.
type streamWriteAccess = writecontrol.Access[streamWriteControlGetter]

type streamWriteControlGetter struct{}

func (streamWriteControlGetter) GetWriteControl(source any) *writecontrol.Control {
	return source.(interface{ streamWriteControl() *writecontrol.Control }).streamWriteControl()
}

func (st *Stream) streamWriteControl() *writecontrol.Control {
	if ctrl := st.writeControl.Load(); ctrl != nil {
		return ctrl
	}
	ctrl := writecontrol.New(true)
	if st.writeControl.CompareAndSwap(nil, ctrl) {
		return ctrl
	}
	return st.writeControl.Load()
}
