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

// Tunnel forwards every connection it accepts to an upstream TCP server: a
// relay with end-to-end backpressure, half-close passthrough and aborts that
// stay aborts. ubridge.Bridge is exactly that shape, so the example is the
// bridge and its configuration.
//
//	  client                        tunnel                      upstream
//	  │                           (ubridge.Bridge)                 │
//	  │  accept  ┌──────────────────────┐                        │
//	  └─────────►│ client leg   upstream│────────────────────────►│
//	   reply ◄───┤    leg         leg   │◄───────────────────────│
//	             └──────────────────────┘                        │
//	                 ▲       ▲   │
//	                 │       │   │ both legs speak the same four callbacks:
//	                 │       │   └── OnRead ── WriteTo(peer) ──►
//	                 │       └────── OnBytesWritten: queue drained below
//	                 │               half the limit → Wake the paused leg
//	                 └────────────── the other leg's OnRead, mirroring
//
// One client leg, one dialed upstream leg, paired as the client arrives:
//
// OnOpen          dials the upstream once per client connection, then pairs
//                 the legs. A failed dial reaches the client as a reset.
// OnRead          forwards what arrived with WriteTo(peer) — unless the
//                 peer's queue already holds InboundLimit bytes. Leaving
//                 the bytes unconsumed pauses this leg's socket reads and
//                 lets TCP slow the sender: that is the backpressure.
// OnBytesWritten  once the queue drains below half the limit, wakes the
//                 leg that paused on it and forwarding resumes.
// OnClose         a graceful end already traveled as a FIN from OnRead; an
//                 abort travels as an RST, so the far end sees a reset.
//
// A request and its half-close, in wire order:
//
//	client ── request ──► client leg ══ WriteTo ══► upstream leg ──► service
//	client ◄── reply ───── client leg ◄══ WriteTo ═══ upstream leg ◄── service
//	client ── FIN ───────► client leg ── CloseWrite ─► upstream leg ─► FIN
//	client ◄── EOF ──────── client leg ◄─ FIN ─────── upstream leg ◄── FIN

package main

import (
	"flag"
	"fmt"

	"github.com/urpc/uio/ubridge"
)

func main() {
	listen := flag.String("listen", ":9528", "address the tunnel listens on")
	upstream := flag.String("upstream", "127.0.0.1:9527", "address of the upstream server")
	flag.Parse()

	// The bridge dials one upstream per client leg and relays both ways; its
	// defaults (a 256 KiB InboundLimit and the Events they configure) are all
	// this relay needs.
	br := &ubridge.Bridge{Upstream: *upstream}
	fmt.Println("tunnel", *listen, "->", *upstream)
	// Serve blocks on the user listener; every accepted connection is
	// relayed by the bridge until the process stops.
	if err := br.Events().Serve(*listen); err != nil {
		fmt.Println("tunnel exited with error:", err)
	}
}
