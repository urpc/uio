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

package ukcp

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/urpc/uio"
)

// ErrServerStarted reports a second Serve on a server that already serves.
var ErrServerStarted = errors.New("ukcp: server already started")

// Server serves KCP over a UIO transport of its own. Configure the exported
// fields before Serve; Serve freezes the configuration. Every connection's
// callbacks reach the Handler it was configured with. There is no built-in
// TLS — KCP has no such standard; put a utls layer above ServerLayer in a
// WithLayers chain if one is wanted.
type Server struct {
	Options

	mu      sync.Mutex
	events  *uio.Events
	addrs   []net.Addr
	started bool
	closed  bool
}

// NewServer returns a Server using opts, http.Server-style: the exported
// fields are the configuration, this constructor only fills them in.
func NewServer(opts Options) *Server { return &Server{Options: opts} }

// Addrs returns the addresses the listeners bound — the ports the system
// chose, for addresses with port zero — once Serve has started. Before that
// it reports none.
func (s *Server) Addrs() []net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]net.Addr(nil), s.addrs...)
}

// Serve listens on each address, in Serve's syntax, and serves until Close:
// it blocks, and reports why the transport stopped. An address without a
// scheme listens for UDP, the network KCP runs on commonly. A listener's
// chain is KCP, so every connection the Handler hears is one reliable
// stream. A listen that fails stops the ones made before it and returns its
// cause.
func (s *Server) Serve(addrs ...string) error {
	if len(addrs) == 0 {
		return fmt.Errorf("%w: a Server needs an address", ErrInvalidOptions)
	}
	resolved, err := s.Options.resolve()
	if err != nil {
		return err
	}
	if resolved.Handler == nil {
		return fmt.Errorf("%w: a Server needs a Handler: reads arrive through it", ErrInvalidOptions)
	}
	if err := resolved.checkTransport(); err != nil {
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	if s.started {
		s.mu.Unlock()
		return ErrServerStarted
	}
	s.started = true

	events := &uio.Events{
		Pollers:                resolved.Pollers,
		ReusePort:              resolved.ReusePort,
		Executor:               resolved.Executor,
		MaxBufferSize:          resolved.MaxBufferSize,
		WriteBufferedThreshold: resolved.WriteBufferedThreshold,
		MaxOutboundBuffered:    resolved.MaxOutboundBuffered,
		AllowHalfOpen:          resolved.AllowHalfOpen,
		Handler:                resolved.Handler,
	}
	chain := []uio.Layer{newLayer(resolved, false)}
	listenOpts := []uio.ListenOption{uio.WithLayers(chain...)}
	if control := resolved.socketControl(); control != nil {
		listenOpts = append(listenOpts, uio.WithListenControl(control))
	}
	var listenErr error
	events.Hooks = uio.FuncHandler{
		Start: func(ev *uio.Events) {
			for _, addr := range addrs {
				ln, err := ev.Listen(withKCPScheme(addr), listenOpts...)
				if err != nil {
					listenErr = err
					break
				}
				s.mu.Lock()
				s.addrs = append(s.addrs, ln.Addr())
				s.mu.Unlock()
			}
			if listenErr != nil {
				_ = ev.Close(listenErr)
			}
		},
	}
	s.events = events
	s.mu.Unlock()

	err = events.Serve()
	if listenErr != nil {
		return listenErr
	}
	return err
}

// Close stops the server: its listeners and its connections end, and a Serve
// running on it returns. Closing again does nothing.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	events := s.events
	s.mu.Unlock()
	if events == nil {
		return nil
	}
	return events.Close(nil)
}
