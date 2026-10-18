//go:build !race

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

// Inbound access is a contract of the API documentation — the inbound buffer
// (Peek, PeekChunk, Discard, Read, InboundBuffered, WriteTo, and the slices
// they return) may only be used inside the current invocation of this
// connection's OnOpen, OnInbound, OnData or OnClose callback — and the
// isolation behind it is the connection turn's serialization: one connection
// runs at most one callback at a time, on one goroutine. The scope
// diagnostics that would catch a violation (see conn_access_debug.go) run
// only in race-enabled builds; here they compile to nothing, so a violating
// call proceeds instead of panicking.

// inboundScope takes no space where the diagnostics are compiled out.
type inboundScope struct{}

func (fc *commonConn) beginInboundCallback() bool { return false }

func (fc *commonConn) endInboundCallback(bool) {}

func (fc *commonConn) assertInboundAccess() {}
