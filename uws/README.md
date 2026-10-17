<p align="center">
  <img src="assets/banner.svg" alt="uws: WebSocket for Go, on uio" width="100%">
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/urpc/uio/uws"><img src="https://pkg.go.dev/badge/github.com/urpc/uio/uws.svg" alt="Go Reference"></a>
  <a href="autobahn/README.md"><img src="https://img.shields.io/badge/Autobahn-0%20failed%20of%20517-22D3EE" alt="Autobahn"></a>
  <a href="https://github.com/urpc/uio/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue.svg" alt="License"></a>
</p>

**uws** is a WebSocket server and client for Go, built on the
[uio](https://github.com/urpc/uio) event engine. The handshake, frame parsing
and your handler all run in the connection's own turn on a shared worker pool,
so a connection costs no goroutine of its own, and a message that arrives in one
frame reaches you straight from the read buffer.

## Highlights

- **Standards first.** RFC 6455 framing and close handshake, RFC 7692
  permessage-deflate, and a clean run of the
  [Autobahn test suite](autobahn/README.md).
- **Three ways in.** A native server that parses the upgrade itself, an
  `http.Handler` for existing `net/http` servers, and a client.
- **Zero-copy reads.** Frames are unmasked in place, and an unfragmented,
  uncompressed message is handed to `OnMessage` straight from the read buffer.
- **Send from anywhere.** `SendText`, `SendBinary` and `Ping` are safe from any
  goroutine and keep their order; they never block on the socket.
- **Batched replies.** Small replies to the frames of one read are encoded
  together and handed to the socket as one write.
- **Bounded by design.** Limits on frame and message size, outbound queue,
  handshake and close time, with heartbeats built in.

## Install

```bash
go get github.com/urpc/uio/uws
```

## Quick start

```go
package main

import (
	"log"

	"github.com/urpc/uio/uws"
)

type echo struct{}

func (echo) OnOpen(*uws.Conn)                  {}
func (echo) OnClose(*uws.Conn, uws.CloseEvent) {}

func (echo) OnMessage(c *uws.Conn, m uws.Message) {
	if m.Type == uws.TextMessage {
		_ = c.SendText(m.Payload)
	} else {
		_ = c.SendBinary(m.Payload)
	}
}

func main() {
	server := uws.NewServer(echo{})
	server.EnableCompression = true
	log.Fatal(server.Serve(":8080"))
}
```

`Serve` takes any number of addresses and returns once every connection has
finished.

## How it works

<p align="center">
  <img src="assets/message-path.svg" alt="A WebSocket message is upgraded, parsed, handled by OnMessage and replied to in one batched write, all in the connection's turn; sends from other goroutines join the same ordered queue" width="100%">
</p>

1. **Upgrade.** `Serve` parses the HTTP upgrade natively; behind `net/http`
   the request is hijacked and the socket moved onto uio.
2. **Parse.** Frames are unmasked in place. Text is validated as UTF-8, with a
   SIMD validator on amd64, and compressed messages are inflated.
3. **Handle.** `OnMessage` runs in the connection's turn. Callbacks of one
   connection are ordered and never overlap.
4. **Reply.** What the handler sends is queued; small replies to one read's
   frames are batched into one write. Sends from other goroutines join the same
   queue in order.

## Behind `net/http`

`Server` is also an `http.Handler`. Start its event loops with `Serve()` (no
address), then mount it like any other handler:

```go
server := uws.NewServer(echo{})
go func() { log.Fatal(server.Serve()) }()

http.Handle("/ws", server)
log.Fatal(http.ListenAndServe(":8080", nil))
```

The handler takes over plain HTTP/1.1 connections. TLS and HTTP/2 connections
cannot be moved onto uio's event loops, so terminate TLS in a proxy in front.
A request that arrives before `Serve` has started gets HTTP 500.

## Client

```go
type client struct{}

func (client) OnOpen(c *uws.Conn)                  { _ = c.SendText([]byte("hello")) }
func (client) OnMessage(c *uws.Conn, m uws.Message) { log.Printf("%s", m.Payload) }
func (client) OnClose(c *uws.Conn, e uws.CloseEvent) {
	if e.Err != nil {
		log.Println("closed:", e.Err)
	}
}

dialer := uws.NewDialer()
if _, err := dialer.Dial(ctx, "ws://127.0.0.1:8080/", client{}); err != nil {
	log.Fatal(err)
}
```

`Dial` returns once the upgrade request is sent and the handshake finishes in
the background. `OnOpen` means the connection is ready; a failed handshake
calls `OnClose` without `OnOpen`, with the cause in `CloseEvent.Err`. The
context bounds the dial and the handshake, not the connection after it.

## Configuration

Every field is optional.

| `Server` field | Default | What it does |
| --- | --- | --- |
| `CheckOrigin` | accept all | Origin policy. Set one for browser-facing servers. |
| `Subprotocols` | none | Subprotocols to negotiate. |
| `MaxFramePayload` | 16 MiB | Largest frame read; larger outgoing messages are fragmented. |
| `MaxMessageSize` | 64 MiB | Largest message, after decompression. |
| `EnableCompression` | off | Negotiates permessage-deflate. |
| `CompressionLevel` | default | flate level when compression is on. |
| `AllowCompressionContextTakeover` | off | Keeps compression history between messages, at more memory per connection. |
| `HandshakeTimeout` | 10 s | Bounds the HTTP upgrade. |
| `CloseTimeout` | 5 s | Bounds the close handshake. |
| `HeartbeatInterval` | off | Sends a ping at this interval. |
| `HeartbeatTimeout` | 2 × interval | Closes the connection when a ping gets no pong in time. |
| `MaxHeaderBytes` | 32 KiB | Upgrade header limit for `Serve`. |
| `DisableUTF8Check` | off | Skips text validation for trusted peers. Not RFC compliant. |
| `Events` | uio defaults | The underlying [`uio.Events`](https://github.com/urpc/uio#configuration): event loops, buffers, outbound limit. uws turns on `WriteBufferedThreshold` at 4 KiB unless you set it; a negative value turns it off. |

`Dialer` has the same limits, compression, timeout and `Events` fields for the
client side.

## Rules of the road

- **Payloads are borrowed.** `Message.Payload` is valid until `OnMessage`
  returns. Call `Message.Clone` to keep it or hand it to another goroutine.
- **Accepted is not delivered.** A nil error from `SendText`, `SendBinary` or
  `Ping` means the message was queued in order, not that the peer has it.
  Past `Events.MaxOutboundBuffered` they return `ErrBackpressure`; size that
  limit to fit the largest message you send.
- **State without locks.** Callbacks of one connection never overlap, so state
  kept with `SetUserdata` needs no locking from them.
- **Closing.** `Conn.Close(code, reason)` sends its close frame after everything
  already accepted and waits for the peer, up to `CloseTimeout`. Protocol
  errors close with 1002, invalid UTF-8 with 1007, oversized messages with
  1009.
- **Lifecycle.** `Server.Close` and `Dialer.Close` request shutdown and return
  at once, so they are safe inside callbacks. A server or dialer cannot be
  restarted.

The [API reference](https://pkg.go.dev/github.com/urpc/uio/uws) documents every
type and method.

## License

[Apache License 2.0](https://github.com/urpc/uio/blob/main/LICENSE)

The Go gopher in the banner is based on the original design by
[Renée French](https://reneefrench.blogspot.com/), licensed under
[CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).
