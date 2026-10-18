//go:build windows || stdio

package uio

import "github.com/urpc/uio/internal/poller"

// prepareAccepted has nothing to do here: std listeners configure accepted
// sockets through the net package before the connection reaches a loop.
func (conn *fdConn) prepareAccepted() {}

// Blocking backends watch every descriptor on its owning loop, untagged.
func (conn *fdConn) watcher() *poller.Poller { return conn.loop.poller }
func (conn *fdConn) assignWatchTag() uint32  { return 0 }

// Blocking backends start a connection's I/O only after OnOpen, so its open
// event cannot race with readiness.
func (conn *fdConn) markOpenPending()             {}
func (conn *fdConn) clearOpenPending()            {}
func (conn *fdConn) noteIO(uint32) bool           { return false }
func (conn *fdConn) skipsEdge(poller.Events) bool { return false }
func (conn *fdConn) RunTask()                     {}
func (conn *fdConn) ShardKey() uint32             { return 0 }
func (conn *fdConn) handleIOSubmitFailure(error)  {}
