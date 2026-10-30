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

// Umux-tunnel relays TCP through one multiplexed transport: the agent dials
// the relay and keeps that transport open; every user connection the relay
// accepts becomes one umux stream on it, and the agent bridges the stream to
// its local service. Half-closes travel through the streams, and an abort on
// one side stays an abort on the other.
//
// Both ends are plain uio.Bridge values whose legs are uio.Conns — user
// sockets on one end, umux streams in the middle — so the two bridges chain
// without a socket between them:
//
//	user ──TCP──► relay users :8080
//	              └─ Bridge A: user leg ⇄ stream leg (one Open per user)
//	                   ══ one transport ══ (umux session, :9000)
//	                                          └─ Bridge B: stream leg ⇄ target leg
//
// The relay accepts users and opens one stream per user on the agent's
// transport session; the agent accepts every stream and dials the local
// service for it. Each bridge passes a half-close on and keeps an abort an
// abort, so the user connection sees the service's behavior unchanged.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/umux"
)

var (
	mode      = flag.String("mode", "relay", "relay|agent")
	listen    = flag.String("listen", ":8080", "relay: address for user connections")
	transport = flag.String("transport", ":9000", "relay: address the agent dials; agent: relay address to dial")
	target    = flag.String("target", "127.0.0.1:22", "agent: local service each stream is bridged to")
)

func main() {
	flag.Parse()
	switch *mode {
	case "relay":
		runRelay()
	case "agent":
		runAgent()
	default:
		fmt.Println("unknown -mode:", *mode)
		os.Exit(2)
	}
}

// runRelay accepts user connections and opens one stream per connection on
// the agent's transport session.
func runRelay() {
	// One bridge for the user side: it accepts the user connections, and its
	// Dial opens each one's upstream leg — a stream on the agent's transport —
	// instead of dialing a socket.
	var br *uio.Bridge
	var sess atomic.Value // *umux.Session: the latest agent transport
	br = &uio.Bridge{Dial: func(ctx context.Context, client uio.Conn, _ uio.DialFunc) (uio.Conn, error) {
		s, _ := sess.Load().(*umux.Session)
		if s == nil {
			return nil, errors.New("umux-tunnel: no agent connected")
		}
		// Bound the open: a stalled agent must not pin the user connection.
		openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		st, err := s.Open(openCtx)
		if err != nil {
			return nil, err
		}
		// Pair instead of letting OnOpen dial: the stream's callbacks reach
		// this bridge through its session, and the user leg is already here.
		br.Pair(client, st)
		return st, nil
	}}

	// The user-side Events belong to the bridge: it accepts, pairs and
	// relays every user connection.
	users := br.Events()
	// The transport side: one umux session per agent, whose streams are
	// opened by the Dial above.
	srv := umux.NewServer(umux.Config{Handler: br, Observer: br})
	go func() {
		// Keep the latest agent session; Sessions closes when the server
		// stops. A reconnecting agent simply replaces it.
		for s := range srv.Sessions() {
			sess.Store(s)
		}
	}()
	go func() { _ = users.Serve("tcp://" + *listen) }()

	fmt.Printf("umux-tunnel relay: users on %s, transports on %s\n", *listen, *transport)
	// Serve the transport listener on the main goroutine: the relay runs
	// until the process stops.
	if err := srv.Serve("tcp://" + *transport); err != nil {
		fmt.Println("relay transport exited:", err)
	}
}

// runAgent dials the relay and bridges every stream it accepts to the local
// target: the bridge's default Dial per accepted stream.
func runAgent() {
	// Every stream the relay opens arrives here as a client leg; the bridge
	// dials the local target for it and relays both ways, so each stream is
	// one TCP connection to the service.
	br := &uio.Bridge{Upstream: *target}
	targets := br.Events()
	// The dial-only Events must already be serving when the first stream
	// asks it to dial the target: wait for its start hook before dialing the
	// relay. The bridge's own hooks stay in the chain.
	started := make(chan struct{})
	hooks := targets.Hooks
	targets.Hooks = uio.FuncHandler{
		Start: func(ev *uio.Events) { hooks.OnStart(ev); close(started) },
		Stop:  func(ev *uio.Events) { hooks.OnStop(ev) },
	}
	go func() { _ = targets.Serve() }()
	<-started

	// Dial the relay's transport; it may still be starting, so retry for a
	// bounded time before giving up.
	cli := umux.NewClient(umux.Config{Handler: br, Observer: br})
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := cli.Dial(context.Background(), "tcp://"+*transport); err == nil {
			break
		} else if time.Now().After(deadline) {
			fmt.Println("agent:", err)
			os.Exit(1)
		}
		time.Sleep(50 * time.Millisecond)
	}

	fmt.Printf("umux-tunnel agent: relay %s -> %s\n", *transport, *target)
	select {} // the session lives as long as the process: keep serving
}
