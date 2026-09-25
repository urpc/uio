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
server.Events.MaxOutboundBuffered = int(uws.DefaultMaxFramePayload) + 14
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

`Server.Serve` accepts multiple listening addresses, or no address for use as
an `http.Handler`.

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
`Close`. Their `Close` methods request transport shutdown without waiting for
the calling connection task, so they are safe inside a callback.
`Server.Serve` is the server join point and returns only after all event loops
and connection tasks have exited. Connection completion and its cause are
reported by `OnClose` and `CloseEvent`. Use `Userdata` and `SetUserdata` for
connection-specific state from ordered connection callbacks; synchronize
access when using other goroutines.

Callbacks run in the owning UIO connection task and messages are ordered per
connection. `Message.Payload` is borrowed until `OnMessage` returns; call
`Message.Clone` before retaining it or passing it to another goroutine.

`SendText`, `SendBinary`, and `Ping` are non-blocking. They return
`uws.ErrBackpressure` when `Events.MaxOutboundBuffered` is full and
`uws.ErrWriteBusy` while a streaming Writer owns the connection. Ordinary
message sends are accepted into the transport and flushed automatically at the
current or next connection-task boundary. Use `BeginMessage` for large or
fragmented messages and call `Writer.Close` after successful writes. A
`Writer.Write` error aborts the connection and releases write ownership
immediately; a later `Close` only returns the first error. Configure
connection-level outbound backpressure with `Events.MaxOutboundBuffered`; zero
disables the transport limit. Size it to hold the largest frame that a callback
may need to buffer.

UWS defaults a zero `Events.WriteBufferedThreshold` to 4 KiB so consecutive
small frames can share transport storage and a flush. Ping, Close, and
`Writer.Close` still establish explicit flush boundaries. An explicit nonzero
threshold is preserved; use a negative value to disable this small-frame
coalescing policy.

On native Unix transports, UIO runs the complete per-connection path as one
serialized connection task: socket I/O, WebSocket parsing and decompression,
then `OnOpen`, `OnMessage`, or `OnClose`. A slow connection therefore does not
block the event loop or callbacks for other connections. UWS does not add a
second executor mailbox or copy message payloads between schedulers.

Set `server.Events.Executor` or `dialer.Events.Executor` to provide the UIO
connection-task scheduler. Without one, UIO uses taskgo with a resident target
of roughly `2 * runtime.GOMAXPROCS(0)` workers, a `512 * runtime.GOMAXPROCS(0)` ceiling
for queued slow callbacks, and a 30-second idle retention window.
The executor's typed `Submit` and `SubmitBatch` methods must return promptly and
must not execute tasks inline; rejection closes the affected connection. UIO
A custom taskgo scheduler can still be installed directly:

```go
workers := runtime.NumCPU() * 512
executor := taskgo.NewTask[uio.IOTask](
	func(task uio.IOTask) { task.RunTask() },
	taskgo.WithConcurrency(workers),
	taskgo.WithMaxIdle(30*time.Second),
	taskgo.WithMaxPending(10128), // connection runners plus scheduling headroom
)
server.Events.Executor = executor
defer executor.Stop(context.Background())
```

Keep the pending limit larger than the expected number of simultaneously ready
connections. UIO applies a per-connection transport outbound budget and pauses
reads when that connection exhausts its budget. Worker needs
depend on the ratio and duration of blocking callbacks; size the pool from a
production profile rather than connection count alone. The stdio/Windows
backend keeps its dedicated per-connection blocking read/write goroutines and
delivers callbacks synchronously from the read path.

`Close` starts a graceful close handshake and returns after queueing its frame.
The transport then waits for the peer response, bounded by `CloseTimeout`.
Protocol errors are reported as close code 1002; invalid UTF-8 uses 1007;
oversized messages use 1009.

`HandshakeTimeout` closes connections that do not complete the HTTP upgrade in
time. This bounds per-connection handshake state and protects the descriptor
budget from slow or deliberately incomplete handshakes.

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

Compare UWS backends and other libraries with the same client version, worker
count, payload, connection count, CPU affinity, and warmup. Do not use a
single run as a performance claim; report multiple runs and include P99
latency. For a like-for-like comparison, assign disjoint CPU sets to the
server and load generator, then give events the same poller count as the
stdio server's `GOMAXPROCS`. In the current 24-CPU Linux test, events slightly
outperforms stdio while using substantially less memory. Increasing events
or stdio `MaxBufferSize` from 4 KiB to 16 KiB did not materially improve the
tested 1 KiB WebSocket pipeline workload; stdio also retained more memory at
16 KiB. Buffer sizing should follow the actual payload and protocol. Measure
both on the deployment platform.
