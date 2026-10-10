package uio

// deadlineKind selects which deadline fields and timer generations change.
type deadlineKind uint8

const (
	deadlineBoth deadlineKind = iota
	deadlineRead
	deadlineWrite
)

// socketOptionKind selects the socket option a setter applies.
type socketOptionKind uint8

const (
	optionLinger socketOptionKind = iota
	optionNoDelay
	optionKeepAlive
	optionKeepAlivePeriod
	optionReadBuffer
	optionWriteBuffer
)
