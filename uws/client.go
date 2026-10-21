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

package uws

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// DefaultDialer is the Dialer the package-level Dial uses. It runs with the
// default options and lives until the process ends, like http.DefaultClient;
// close it and every connection it made by calling DefaultDialer.Close.
var DefaultDialer = &Dialer{}

// clientQueueMessages bounds how many received messages a Client holds before
// its connection stops reading; see Client.ReadMessage.
const clientQueueMessages = 64

// Dial connects with DefaultDialer and returns the client once the handshake
// completes. See Dialer.DialClient for the contract.
func Dial(ctx context.Context, addr string) (*Client, error) {
	return DefaultDialer.DialClient(ctx, addr)
}

// Client is a WebSocket client connection whose messages are read on demand:
// DialClient hands one over after the handshake, SendText and SendBinary
// write, and ReadMessage returns the next received message. The connection
// must be read continuously: a Client whose messages are not being read
// stops reading, so the peer's flow control slows it and its control frames,
// Ping included, go unanswered. Client.Close always ends a Client, however
// many messages are waiting.
type Client struct {
	*Conn

	// queue carries messages from the connection's OnMessage to ReadMessage.
	// A full queue blocks OnMessage, which is the backpressure: the
	// connection stops reading until the application catches up.
	queue chan Message
	// done is closed once no more input is accepted, by Client.Close or by
	// the connection's OnClose. It releases an OnMessage blocked on a full
	// queue, and ends ReadMessage once the queue has drained.
	done     chan struct{}
	doneOnce sync.Once
	// ended is closed after the connection's OnClose stored its event, so a
	// ReadMessage that drained the queue never reports an empty close.
	ended chan struct{}
	event atomic.Pointer[CloseEvent]
}

// CloseError reports how a Client's connection ended. ReadMessage returns it
// after the connection closed and every queued message was returned.
type CloseError struct {
	CloseEvent
}

func (e *CloseError) Error() string {
	if e.Err != nil {
		return "uws: connection closed: " + e.Err.Error()
	}
	return "uws: connection closed"
}

// Unwrap exposes the transport, protocol or application error in CloseEvent,
// so a deadline or cancellation that ended the dial still matches with
// errors.Is.
func (e *CloseError) Unwrap() error { return e.Err }

// ReadMessage returns the next received message, waiting for ctx to allow one.
// It reports a *CloseError after the connection closed and every message that
// was received before that has been returned; a canceled or timed-out ctx
// returns its error with the connection unaffected.
func (c *Client) ReadMessage(ctx context.Context) (Message, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case message := <-c.queue:
		return message, nil
	case <-c.done:
		// Messages received before the close still come out first.
		select {
		case message := <-c.queue:
			return message, nil
		default:
		}
		if event := c.event.Load(); event != nil {
			return Message{}, &CloseError{CloseEvent: *event}
		}
		// The close is still finishing; its event is set before ended.
		select {
		case <-c.ended:
			return Message{}, &CloseError{CloseEvent: *c.event.Load()}
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

// CloseEventInfo reports how the connection ended. It is meaningful once
// ReadMessage has returned a CloseError, or after Close returned and the
// connection finished closing.
func (c *Client) CloseEventInfo() CloseEvent {
	if event := c.event.Load(); event != nil {
		return *event
	}
	return CloseEvent{}
}

// Close closes the connection with code and reason. It also ends reading:
// messages still in the queue are returned by further ReadMessage calls until
// they run out, and an OnMessage stuck on a full queue gives up at once. An
// invalid code or reason returns the error with the connection untouched,
// still reading.
func (c *Client) Close(code uint16, reason string) error {
	if _, err := c.Conn.closePayload(code, reason); err != nil {
		return err
	}
	c.finish()
	return c.Conn.Close(code, reason)
}

// finish ends message intake exactly once.
func (c *Client) finish() {
	c.doneOnce.Do(func() { close(c.done) })
}

// newClient is the Client a DialClient wires its internal handler to.
func newClient() *Client {
	return &Client{
		queue: make(chan Message, clientQueueMessages),
		done:  make(chan struct{}),
		ended: make(chan struct{}),
	}
}

// enqueue offers a message to the reader, or gives up once intake ended.
func (c *Client) enqueue(message Message) {
	select {
	case c.queue <- message:
	case <-c.done:
	}
}

// pullHandler is the Handler a DialClient installs in place of the dialer's
// own: it reports the handshake to the dialing goroutine and feeds the
// client's queue.
type pullHandler struct {
	client *Client
	// ready receives the handshake outcome: nil after OnOpen, the close
	// otherwise. One receiver, so one buffer never blocks a callback.
	ready chan error
}

func (h *pullHandler) OnOpen(conn *Conn) {
	h.client.Conn = conn
	h.ready <- nil
}

func (h *pullHandler) OnMessage(_ *Conn, message Message) {
	h.client.enqueue(message.Clone())
}

func (h *pullHandler) OnClose(_ *Conn, event CloseEvent) {
	h.client.event.Store(&event)
	h.client.finish()
	close(h.client.ended)
	select {
	case h.ready <- &CloseError{CloseEvent: event}:
	default:
	}
}

// DialClient connects and returns the client once the handshake completes,
// ready to send and read: the pull-mode counterpart of Dial, which reports
// connection events to Options.Handler instead. The dialer's Handler must be
// nil, since the client takes the callbacks; DialClient returns
// ErrInvalidOptions otherwise. ctx bounds the TCP connection and the
// handshake, not the connection after it.
func (d *Dialer) DialClient(ctx context.Context, addr string) (*Client, error) {
	if d.Options.Handler != nil {
		return nil, fmt.Errorf("%w: Handler is set: DialClient owns the callbacks", ErrInvalidOptions)
	}
	client := newClient()
	handler := &pullHandler{client: client, ready: make(chan error, 1)}
	if err := d.dialWithHandler(ctx, addr, nil, handler); err != nil {
		// An error means no connection was made and no callback follows.
		return nil, err
	}
	// The handshake completes asynchronously: OnOpen lets DialClient return,
	// a failed one reports OnClose here.
	if err := <-handler.ready; err != nil {
		return nil, err
	}
	return client, nil
}
