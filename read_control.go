package uio

// ReadController pauses the delivery of one connection's input.
//
// While read is paused the connection's OnRead does not run; OnTimer, writes
// and close work go on. Input already received is kept and handed to the first
// OnRead after ResumeRead, before the end of the stream is reported or the
// connection closes for it. A plain stream stops reading its socket too, so
// TCP flow control slows the peer at once; a layered stack reads on until its
// inbound bounds are reached. Pausing does not interrupt a callback or a
// socket read already in flight: a protocol handling several messages in one
// callback checks ReadPaused between them.
//
// A fatal transport error, a reset or a local close still ends a paused
// connection — on the portable stdio backend a reset behind a pause is seen
// once reading resumes or a read deadline passes, the socket not being read
// meanwhile. Datagram connections report ErrUnsupported: their peers share
// one socket. All methods are safe from any goroutine, callbacks included.
type ReadController interface {
	// PauseRead pauses input delivery. It is the application's one switch:
	// idempotent, not reference counted. Protocol layers and helpers such as
	// Bridge hold pauses of their own, which this switch neither sets nor
	// clears.
	PauseRead() error
	// ResumeRead releases the application's pause. Once no pause remains, a
	// turn hands the retained input to OnRead and reading resumes. Without a
	// pause to release it does nothing; to get another OnRead for input a
	// callback left unconsumed, call Wake.
	ResumeRead() error
	// ReadPaused reports whether any pause, the application's or another
	// owner's, holds the connection's input. A full inbound buffer is not a
	// pause: its consumer must still be able to drain it.
	ReadPaused() bool
}
