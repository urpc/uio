// Package writecontrol shares outbound-progress notifications between the
// transport and protocol packages without adding public factory methods.
package writecontrol

// Getter locates the write control belonging to the original dynamic output
// boundary. Implement it on a private zero-sized type in the boundary's owning
// package.
type Getter interface {
	GetWriteControl(source any) *Control
}

// TurnReserver reserves turn-local contiguous outbound space without public
// methods on uio.Conn or uio.Lower.
type TurnReserver interface {
	ReserveTurnWrite(n int) ([]byte, bool, error)
}

// Access exposes only a package-private capability. Embedding this zero-sized
// value first in a connection preserves its layout and lets sibling packages
// subscribe to outbound progress without adding public methods. The original
// source is passed through so a getter can support promoted access from
// concrete wrappers.
type Access[T Getter] struct{}

func (Access[T]) writeControlBoundary(source any) *Control {
	var getter T
	return getter.GetWriteControl(source)
}

// ControlOf resolves source's shared write control, or nil if unsupported.
func ControlOf(source any) *Control {
	if access, ok := source.(interface{ writeControlBoundary(any) *Control }); ok {
		return access.writeControlBoundary(source)
	}
	return nil
}

// Watch registers fn on source's write control and reports whether source
// supports write-progress notifications.
func Watch(source any, fn func(sent int)) bool {
	c := ControlOf(source)
	if c == nil || fn == nil {
		return false
	}
	c.Watch(fn)
	return true
}
