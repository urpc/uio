//go:build windows || stdio

package uio

import "github.com/urpc/uio/internal/poller"

const ioEventOpen uint32 = 1 << 16
const ioEventRead uint32 = 1
const ioEventWake uint32 = 1 << 17

// prepareAccepted has nothing to do here: std listeners configure accepted
// sockets through the net package before the connection reaches a loop.
func (conn *fdConn) prepareAccepted() {}

// Blocking backends watch every descriptor on its owning loop, untagged.
func (conn *fdConn) watcher() *poller.NetPoller { return conn.loop.poller }
func (conn *fdConn) assignWatchTag() uint32     { return 0 }

func (conn *fdConn) scheduleIO(events uint32) {
	if events&ioEventOpen != 0 {
		conn.fireOnOpen()
	}
}

// Blocking backends start a connection's I/O only after OnOpen, so its open
// event cannot race with readiness.
func (conn *fdConn) markOpenPending()            {}
func (conn *fdConn) clearOpenPending()           {}
func (conn *fdConn) scheduleOpen()               { conn.scheduleIO(ioEventOpen) }
func (conn *fdConn) noteIO(uint32) bool          { return false }
func (conn *fdConn) RunTask()                    {}
func (conn *fdConn) handleIOSubmitFailure(error) {}
func (conn *fdConn) readNeedsRedelivery() bool   { return false }
func (conn *fdConn) clearReadRedelivery() bool   { return false }
func (conn *fdConn) writeIsBlocked() bool        { return false }
