//go:build windows || stdio

package uio

func (conn *fdConn) runUDPWriteTask(owned *Buffer) udpWriteResult {
	ReleaseBuffer(owned)
	return udpWriteResult{err: errUnsupported}
}
