//go:build windows || stdio

/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

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
