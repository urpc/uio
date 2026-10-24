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

package ulayer

import (
	"sync"

	"github.com/urpc/uio"
)

// Func is a Layer whose handler is built per connection: f receives the
// connection below and the handler above and returns the handler that serves
// this one connection below — usually one closing over the conn it hands
// upward, which is how a layer pairs the connection it reads with the
// connection it delivers. A layer with state shared by its connections, a
// session cache, is a struct implementing Layer itself.
type Func func(lower uio.Conn, next uio.Handler) uio.Handler

// Handler implements Layer.
func (f Func) Handler(next uio.Handler) uio.Handler {
	return &funcLayer{next: next, build: f, serving: make(map[uio.Conn]uio.Handler)}
}

// funcLayer routes each connection below to the handler built for it.
type funcLayer struct {
	next    uio.Handler
	build   Func
	mu      sync.Mutex
	serving map[uio.Conn]uio.Handler
}

func (l *funcLayer) handler(lower uio.Conn) uio.Handler {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.serving[lower]
}

func (l *funcLayer) OnOpen(lower uio.Conn) {
	handler := l.build(lower, l.next)
	l.mu.Lock()
	l.serving[lower] = handler
	l.mu.Unlock()
	handler.OnOpen(lower)
}

func (l *funcLayer) OnData(lower uio.Conn) error {
	if handler := l.handler(lower); handler != nil {
		return handler.OnData(lower)
	}
	return nil
}

func (l *funcLayer) OnClose(lower uio.Conn, err error) {
	l.mu.Lock()
	handler := l.serving[lower]
	delete(l.serving, lower)
	l.mu.Unlock()
	if handler != nil {
		handler.OnClose(lower, err)
	}
}
