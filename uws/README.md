# uws

`uws` is an asynchronous WebSocket implementation built on UIO. It implements
RFC 6455 framing and message rules, and RFC 7692 `permessage-deflate`.

## Server

```go
import (
	"context"
	"log"
	"net/http"

	"github.com/urpc/uio/uws"
)

type handler struct{}

func (handler) OnOpen(*uws.Conn) {}

func (handler) OnMessage(conn *uws.Conn, message uws.Message) {
	if message.Type == uws.TextMessage {
		_ = conn.SendText(message.Payload)
	} else {
		_ = conn.SendBinary(message.Payload)
	}
}

func (handler) OnClose(*uws.Conn, uws.CloseEvent) {}

server := uws.NewServer(handler{})
server.CheckOrigin = func(*http.Request) bool { return true }
server.EnableCompression = true
log.Fatal(server.Serve(":8080"))
```

The same Server also implements `http.Handler`. Start its UIO event loops
without a listener before accepting HTTP requests:

```go
server := uws.NewServer(handler{})
go func() { _ = server.Serve() }()

httpServer := &http.Server{
	Addr:    ":8080",
	Handler: server,
}
defer server.Close(nil)
log.Fatal(httpServer.ListenAndServe())
```

Calling `ServeHTTP` before `Serve` is ready returns HTTP 500 and does not start
UIO implicitly. The Handler adapter supports plain HTTP/1.1 TCP connections. It
validates the request, hijacks it, rejects any client bytes already buffered
after the HTTP header, and transfers the socket to UIO before sending the 101
response. Local TLS termination and HTTP/2 cannot be transferred to the native
poller; place a TLS proxy in front of the HTTP server when those protocols are
required. The application remains responsible for shutting down its
`http.Server`, while `Server.Close` stops UWS connections and UIO event loops.
Configure `http.Server.MaxHeaderBytes` for this path; `Server.MaxHeaderBytes`
applies to the native `Server.Serve` parser.

Set `CheckOrigin` to an application-specific policy for browser-facing
servers; a nil policy accepts all origins.

Servers and dialers are single-lifecycle objects and cannot be restarted after
`Close`. Their `Close` methods request transport shutdown without waiting on the
calling event loop, so they are safe inside a synchronous callback.
`Server.Serve` is the server join point and returns only after all event loops
have exited. Connection completion and its cause are reported by `OnClose` and
`CloseEvent`. Use `Userdata` and `SetUserdata` for connection-specific state
from ordered connection callbacks; synchronize access when using other
goroutines.

Callbacks run in the owning UIO event loop and messages are ordered per
connection. `Message.Payload` is borrowed until `OnMessage` returns; call
`Message.Clone` before retaining it or passing it to another goroutine.

`SendText`, `SendBinary`, and `Ping` are non-blocking. They return
`uws.ErrBackpressure` when the configured outbound budget is full. Ordinary
message sends are accepted into the transport and flushed automatically at its
callback or task boundary. Use `BeginMessage` for large or fragmented messages
and call `Writer.Close` after successful writes. A `Writer.Write` error aborts
the connection and releases write ownership immediately; a later `Close` only
returns the first error. Set `MaxOutboundBytes` to a negative value to disable
the UWS outbound byte limit; zero uses the safe default.

UWS defaults a zero `Events.WriteBufferedThreshold` to 4 KiB on every
transport so consecutive small writes can share a transport flush. Ping, Close,
and `Writer.Close` still establish explicit flush boundaries. An explicit
nonzero threshold is preserved; use a negative value to keep immediate-write
mode.

Handlers normally run on the I/O event loop and should return promptly. Set
`Server.Executor` or `Dialer.Executor` to move `OnOpen`, `OnMessage`, and
`OnClose` to an application executor. Callbacks for one connection remain
serialized. The executor's `Submit` method must return promptly and return
`false` when its bounded queue is full; use a fixed worker pool for the
application work. UWS keeps generous internal mailbox limits as a final guard
against an executor that stops making progress. An overloaded connection is
closed with code 1013 and `ErrApplicationBackpressure`; other connections
continue to run.
If `Submit` rejects a callback, that connection is closed with
`ErrExecutorRejected`. UWS never falls back to running a rejected callback on
the I/O loop, so remaining callbacks, including `OnClose`, are dropped. Size
the executor for the expected concurrent connections, or provide a fair
scheduler in front of a bounded worker pool.

`*taskgo.Queue` implements this executor interface directly (taskgo's
`Submit` API). A typical setup for deep, bursty business calls is:

```go
executor := taskgo.New(
	taskgo.WithConcurrency(8),
	taskgo.WithMaxIdle(time.Second),
	taskgo.WithMaxPending(10128), // connection runners plus scheduling headroom
)
server.Executor = executor
defer executor.Stop(context.Background())
```

Keep the taskgo pending limit larger than the expected number of simultaneously
scheduled connection runners (leave headroom for runner replacement while a
worker callback is still returning). UWS independently bounds queued messages
with its per-connection and server-wide mailbox limits.

`Close` sends a close frame and waits for the peer response, bounded by
`CloseTimeout`. Protocol errors are reported as close code 1002; invalid UTF-8
uses 1007; oversized messages use 1009.

`HandshakeTimeout` closes connections that do not complete the HTTP upgrade in
time. This protects the event loop and file-descriptor budget from slow or
deliberately incomplete handshakes.

`Conn.SetDeadline`, `SetReadDeadline`, and `SetWriteDeadline` forward directly
to the UIO transport. Applications own deadline policy; pass a zero time to
clear a deadline.

Compression is negotiated only when `EnableCompression` is set. The default
server policy uses no context takeover to bound per-connection state. Set
`AllowCompressionContextTakeover` when retaining compression history is
acceptable. Window sizes are negotiated according to RFC 7692.

Text messages are validated as UTF-8 as required by RFC 6455. Trusted-peer
deployments may set `DisableUTF8Check` to skip Text Message validation;
close-frame reasons remain validated. Enabling this option is not RFC 6455
compliant.

## Client

```go
import (
	"context"
	"log"

	"github.com/urpc/uio/uws"
)

type clientHandler struct{}

func (clientHandler) OnOpen(conn *uws.Conn) {
	_ = conn.SendText([]byte("hello"))
}

func (clientHandler) OnMessage(conn *uws.Conn, message uws.Message) {}

func (clientHandler) OnClose(conn *uws.Conn, info uws.CloseEvent) {
	if info.Err != nil {
		log.Printf("websocket closed: %v", info.Err)
	}
}

dialer := uws.NewDialer()
_, err := dialer.Dial(context.Background(), "ws://127.0.0.1:8080/", clientHandler{})
if err != nil {
	log.Fatal(err)
}
```

`Dial` reports errors that prevent the connection attempt from starting.
WebSocket handshake completion is asynchronous: `OnOpen` means the connection
is ready for application messages, while a handshake failure calls `OnClose`
without a preceding `OnOpen` and places the cause in `CloseEvent.Err`.

The context passed to `Dial` is checked before UIO starts its network dial and
then bounds the WebSocket handshake. The network connection itself follows
`uio.Events.Dial` semantics. After `OnOpen`, the Dial context no longer affects
the established connection. `OnClose` reports the transport or protocol error
through `CloseEvent.Err`.

## Linux benchmark

Compare UWS, stdio, and Gorilla with the same client, payload, connection
count, CPU affinity, and warmup. Do not use a single run as a performance
claim; report multiple runs and include P99 latency. The UIO Unix path is
intended for bounded coroutine counts, while `stdio` is a portability and
baseline implementation.
