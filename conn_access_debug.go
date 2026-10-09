//go:build race

package uio

// The inbound-scope diagnostics exist only in the race-enabled build. They
// record that a connection's inbound callback is in progress and fail loudly
// when the inbound buffer is touched outside one. They are misuse diagnostics,
// not a safety mechanism: the guarantee that a connection's inbound buffer is
// serialized comes from the connection turn (native) and callbackMu (stdio),
// and these check only that a callback runs — not who calls. A foreign
// goroutine reading while a callback runs passes, and a borrowed slice used
// after its callback returned is not detected. Builds without the race
// detector compile the whole mechanism out; see conn_access_release.go.

// beginInboundCallback marks the connection's inbound callback in progress,
// reporting whether this call entered it, so nested internal helpers keep the
// outer caller's scope intact.
func (fc *commonConn) beginInboundCallback() bool {
	return fc.inboundLive.CompareAndSwap(false, true)
}

func (fc *commonConn) endInboundCallback(started bool) {
	if started {
		fc.inboundLive.Store(false)
	}
}

func (fc *commonConn) assertInboundAccess() {
	// Unregistered connections are used by low-level helpers and tests before an
	// owner exists. Registered connections always have both fields.
	if fc.loop == nil || fc.events == nil {
		return
	}
	if fc.inboundLive.Load() {
		return
	}
	panic("uio: inbound access outside a connection callback")
}
