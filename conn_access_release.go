//go:build !race

package uio

// Inbound access is a contract of the API documentation — the inbound buffer
// (Peek, PeekChunk, Discard, Read, InboundBuffered, WriteTo, and the slices
// they return) may only be used inside the current invocation of this
// connection's OnOpen, OnInbound, OnData or OnClose callback — and the
// isolation behind it is the connection turn's serialization: one connection
// runs at most one callback at a time, on one goroutine. The scope
// diagnostics that would catch a violation (see conn_access_debug.go) run
// only in race-enabled builds; here they compile to nothing, so a violating
// call proceeds instead of panicking.

func (fc *commonConn) beginInboundCallback() bool { return false }

func (fc *commonConn) endInboundCallback(bool) {}

func (fc *commonConn) assertInboundAccess() {}
