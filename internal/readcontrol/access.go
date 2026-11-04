// Package readcontrol shares input-consumer ownership between the transport
// and the protocol packages without exposing owner handles in the public API.
package readcontrol

import "errors"

// ErrUnsupported is shared with uio.ErrUnsupported without an import cycle.
var ErrUnsupported = errors.New("unsupported method")

// Getter locates the control belonging to the original dynamic input boundary.
// Implement it on a private zero-sized type in the boundary's owning package.
// The zero value must be usable and must not create a second control for the
// same input boundary.
type Getter interface {
	GetReadControl(source any) *Control
}

// Access exposes only a package-private capability. Embedding this zero-sized
// value first in a connection preserves its layout and lets sibling packages
// acquire their own pause owner without adding public factory methods. The
// original source is passed through, so a getter can support promoted access
// from wrappers by asserting a private provider interface in its own package.
type Access[T Getter] struct{}

func (Access[T]) readControlBoundary(source any) *Control {
	var getter T
	return getter.GetReadControl(source)
}

// ControlOf resolves an existing boundary's shared control. A custom Conn
// without the private capability returns nil; callers must not substitute the
// application's public pause slot for independently owned internal pauses.
func ControlOf(source any) *Control {
	if access, ok := source.(interface{ readControlBoundary(any) *Control }); ok {
		return access.readControlBoundary(source)
	}
	return nil
}

// NewPause acquires an initially unpaused owner of source's input boundary.
// Unsupported sources return nil; nil handles report ErrUnsupported.
func NewPause(source any) *Pause { return ControlOf(source).NewPause() }
