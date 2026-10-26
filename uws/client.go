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
	"os"
	"sync"
	"sync/atomic"
	"time"
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
// Ping included, go unanswered. Its own sends slow with it: one made while
// the queue is full waits for reading to resume before it leaves. Client.Close
// always ends a Client, however many messages are waiting, and so do the
// closes this end starts itself — Dialer.Close, a deadline, a heartbeat
// timeout, a failed send: a message that arrives once one has begun and finds
// the queue full is dropped rather than holding the close up.
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
	// stop is closed once this end starts closing the connection. It releases
	// an OnMessage blocked on a full queue, which would otherwise keep the
	// connection from ever closing, without ending reading.
	stop     chan struct{}
	stopOnce sync.Once
	// readDeadline is the read deadline set through the Client's Conn. The
	// client owns it — the connection's turn cannot observe the transport's
	// own expiry while its OnMessage waits on a full queue — and its expiry
	// closes the connection, which releases the queue; see setReadDeadline.
	deadlineMu    sync.Mutex
	readDeadline  time.Time
	deadlineTimer *time.Timer
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

// release lets the connection close past a full queue; see stop.
func (c *Client) release() {
	c.stopOnce.Do(func() { close(c.stop) })
}

// setReadDeadline arms the client-owned read deadline; a zero time clears
// it. An expiry the application pushed back since the timer was armed is
// discarded here, and the timer the later deadline armed is already waiting.
func (c *Client) setReadDeadline(deadline time.Time) {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readDeadline = deadline
	if c.deadlineTimer != nil {
		c.deadlineTimer.Stop()
		c.deadlineTimer = nil
	}
	if c.readDeadline.IsZero() || c.closedConn() {
		return
	}
	c.deadlineTimer = time.AfterFunc(time.Until(deadline), c.expireReadDeadline)
}

// expireReadDeadline closes the connection once its read deadline arrived.
// Only the deadline decides: nothing else observes it while a full queue
// holds the connection's turn, so this timer is its one owner.
func (c *Client) expireReadDeadline() {
	c.deadlineMu.Lock()
	if c.readDeadline.IsZero() || c.readDeadline.After(time.Now()) {
		c.deadlineMu.Unlock()
		return
	}
	c.deadlineTimer = nil
	c.deadlineMu.Unlock()
	if conn := c.Conn; conn != nil {
		conn.failStream(os.ErrDeadlineExceeded)
	}
}

// stopDeadlines drops the deadline timer once the connection closed.
func (c *Client) stopDeadlines() {
	c.deadlineMu.Lock()
	c.readDeadline = time.Time{}
	if c.deadlineTimer != nil {
		c.deadlineTimer.Stop()
		c.deadlineTimer = nil
	}
	c.deadlineMu.Unlock()
}

// closedConn reports whether the connection's OnClose has run.
func (c *Client) closedConn() bool {
	select {
	case <-c.ended:
		return true
	default:
		return false
	}
}

// newClient is the Client a DialClient wires its internal handler to.
func newClient() *Client {
	return &Client{
		queue: make(chan Message, clientQueueMessages),
		done:  make(chan struct{}),
		stop:  make(chan struct{}),
		ended: make(chan struct{}),
	}
}

// enqueue offers a message to the reader. With the queue full it waits for
// room — the backpressure that pauses a client which has stopped reading — and
// gives up once intake ended or the connection started closing; from then on
// every message gives up, queued or not, so a close under way loses messages
// whole rather than leaving gaps between delivered ones. The wait also ends at
// the connection's next heartbeat decision: a client nobody reads must not
// block its own turns past it, or the timeout that would close it could never
// run. Past that moment the message is dropped, the round completes, and the
// heartbeat tick that follows pings or expires the connection. The boundary is
// read only when the queue is full, so a reading client pays nothing for it.
func (c *Client) enqueue(message Message, conn *Conn) {
	select {
	case <-c.stop:
		return
	case <-c.done:
		return
	default:
	}
	select {
	case c.queue <- message:
		return
	default:
	}
	until := conn.heartbeatBoundary()
	if !until.IsZero() && !time.Now().Before(until) {
		return // the decision is already due: let the round reach its tick
	}
	var deadline <-chan time.Time
	if !until.IsZero() {
		// A timer this wait stops on the way out: time.After's would keep
		// the runtime's wheel busy until the heartbeat's deadline passed.
		timer := time.NewTimer(time.Until(until))
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case c.queue <- message:
	case <-c.done:
	case <-c.stop:
	case <-deadline:
	}
}

// pullHandler is the Handler a DialClient installs in place of the dialer's
// own: it reports the handshake to the dialing goroutine and feeds the
// client's queue.
type pullHandler struct {
	client *Client
	dialer *Dialer
	// ready receives the handshake outcome: nil after OnOpen, the close
	// otherwise. One receiver, so one buffer never blocks a callback.
	ready chan error
}

func (h *pullHandler) OnOpen(conn *Conn) {
	h.client.Conn = conn
	h.dialer.trackClient(h.client)
	h.ready <- nil
}

func (h *pullHandler) OnMessage(conn *Conn, message Message) {
	h.client.enqueue(message.Clone(), conn)
}

func (h *pullHandler) OnClose(_ *Conn, event CloseEvent) {
	h.dialer.clients.Delete(h.client)
	h.client.stopDeadlines()
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
	handler := &pullHandler{client: client, dialer: d, ready: make(chan error, 1)}
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

// trackClient lets Dialer.Close release client. Registered first and checked
// after, so a client either is seen by the Close or sees it.
func (d *Dialer) trackClient(client *Client) {
	d.clients.Store(client, struct{}{})
	if d.closedCause() != nil {
		client.release()
	}
}

// releaseClients lets every pull client's connection close past its queue.
func (d *Dialer) releaseClients() {
	d.clients.Range(func(key, _ any) bool {
		key.(*Client).release()
		return true
	})
}

// releasePullClient lets a pull Client's connection close past its full
// queue once this end starts closing it.
func (c *Conn) releasePullClient() {
	if h, ok := c.handler.(*pullHandler); ok {
		h.client.release()
	}
}
