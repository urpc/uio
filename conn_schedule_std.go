//go:build windows || stdio

package uio

const ioEventOpen uint32 = 1 << 16
const ioEventRead uint32 = 1
const ioEventWake uint32 = 1 << 17

func (conn *fdConn) scheduleIO(events uint32) {
	if events&ioEventOpen != 0 {
		conn.fireOnOpen()
	}
}
func (conn *fdConn) noteIO(uint32) bool          { return false }
func (conn *fdConn) RunTask()                    {}
func (conn *fdConn) handleIOSubmitFailure(error) {}
func (conn *fdConn) readNeedsRedelivery() bool   { return false }
func (conn *fdConn) clearReadRedelivery() bool   { return false }
func (conn *fdConn) writeIsBlocked() bool        { return false }
