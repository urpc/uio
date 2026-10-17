<p align="center">
  <img src="assets/banner.svg" alt="uio: event-driven networking for Go" width="100%">
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/urpc/uio"><img src="https://pkg.go.dev/badge/github.com/urpc/uio.svg" alt="Go Reference"></a>
  <a href="https://github.com/urpc/uio/actions/workflows/ci.yml"><img src="https://github.com/urpc/uio/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue.svg" alt="License"></a>
</p>

**uio** is an event-driven networking library for Go. You write a few
callbacks; uio watches the sockets, runs your code when a connection has
something to say, and sends your replies in as few system calls as it can —
without a goroutine per connection.

## Highlights

- **Callbacks, not goroutines.** `OnOpen`, `OnData` and `OnClose` run in order
  for each connection, so per-connection state needs no locks, and on
  epoll/kqueue an idle connection is a small struct rather than a parked
  goroutine.
- **A slow handler stalls nothing else.** On epoll/kqueue, event loops only
  collect readiness; callbacks run on a worker pool, and work stuck behind a
  blocked callback is handed to another worker within milliseconds.
- **Write from anywhere.** `Write`, `WriteOwned` and `Close` are safe from any
  goroutine; bytes leave in the order they were written.
- **Zero-copy where it counts.** Decode packets straight from the read buffer,
  encode replies straight into the send queue, and send a round of replies
  together, usually in one `writev`.
- **Built-in backpressure.** Per-connection limits on buffered input and
  output turn a slow or flooding peer into an error instead of unbounded
  memory.
- **Cross-platform.** epoll on Linux, kqueue on macOS and the BSDs, and a
  portable goroutine-per-connection backend on Windows (or anywhere with
  `-tags stdio`).
- **TCP, UDP and Unix sockets**, listening and dialing, IPv4 and IPv6.

## Install

```bash
go get github.com/urpc/uio
```

uio needs Go 1.25 or newer.

## Quick start

An echo server:

```go
package main

import (
	"log"

	"github.com/urpc/uio"
)

func main() {
	var events uio.Events

	events.OnOpen = func(c uio.Conn) {
		log.Println("hello", c.RemoteAddr())
	}

	events.OnData = func(c uio.Conn) error {
		_, err := c.WriteTo(c) // echo everything that arrived
		return err
	}

	events.OnClose = func(c uio.Conn, err error) {
		log.Println("bye", c.RemoteAddr(), err)
	}

	log.Fatal(events.Serve(":9527"))
}
```

`Serve` accepts several addresses at once, with an optional scheme:
`":9527"`, `"tcp6://[::1]:9527"`, `"udp://:5353"`, `"unix:///tmp/app.sock"`.
Once `Serve` is running (`Serve()` with no address for a dial-only client),
the same `Events` can dial out with `Dial` or `DialContext`, or take over an
established stream `net.Conn` with `Adopt`.

## How it works

<p align="center">
  <img src="assets/architecture.svg" alt="Event loops collect readiness, a sharded scheduler queues each connection on its own core, a connection turn reads and runs your callbacks, and a write turn sends writes from other goroutines" width="100%">
</p>

1. **Watch.** A few event loops — epoll or kqueue instances, one per four Ps
   by default — accept connections and collect readiness. They never run your
   code.
2. **Queue.** A ready connection is handed to a sharded worker pool. A
   connection always lands on the same shard, so its work tends to return to
   the same CPU, where its socket is still warm in cache. You can supply your
   own scheduler through `Events.Executor`.
3. **Serve.** A connection's *turn* reads the socket, runs `OnData`, and then
   flushes every reply the callback wrote in one go. Turns of one connection
   never overlap; different connections run in parallel.

A write from another goroutine — say, an RPC reply that finishes later — is
queued and sent by a short *write turn*, so it does not wait for a slow read.
Only one sender touches a socket at a time, and bytes leave in the order they
were written.

This describes the native epoll/kqueue backend. The portable `stdio` backend
used on Windows (or with `-tags stdio`) gives each stream connection its own
reader and writer goroutines instead.

## Configuration

Everything is optional; the zero `Events` is ready to use.

| Field | Default | What it does |
| --- | --- | --- |
| `Pollers` | one per 4 Ps, 1–8 | Number of event loops, never more than `GOMAXPROCS`. |
| `Executor` | built-in sharded pool | Your own scheduler for connection and write turns (native backend). |
| `MaxBufferSize` | 4 KiB | Bytes read per socket read. |
| `WriteBufferedThreshold` | off | Native backend: callback writes smaller than this are queued for the end of the turn instead of sent at once. Replies written in `OnData` are batched either way. |
| `MaxOutboundBuffered` | off | Per-connection limit on unsent bytes; a write past it returns `ErrOutboundOverflow`. |
| `MaxInboundBuffered` | off | Per-connection limit on unread bytes; past it the connection closes with `ErrInboundOverflow`. |
| `ReusePort` | off | Sets `SO_REUSEPORT` on listeners. |
| `LockOSThread` | off | Locks each event-loop goroutine to its own OS thread; callback workers are unaffected. |

Besides the three main callbacks there are `OnInbound` and `OnOutbound`, which
report bytes read and written, and `OnStart` and `OnStop` for the server's
lifecycle.

## Going faster

These are optional. Plain `Read`, `Write` and `WriteTo` work on every backend.

**Decode in place.** `PeekChunk` returns the first chunk of buffered bytes
without copying.
For a length-prefixed protocol, `ReserveInbound` gathers a packet that spans
several reads into one chunk, so it can still be decoded in place:

```go
events.OnData = func(c uio.Conn) error {
	var header [4]byte
	for {
		h := c.Peek(header[:]) // copies only if the header spans reads
		if len(h) < 4 {
			return nil // wait for the header
		}
		size := binary.BigEndian.Uint32(h)
		if size > maxPacket {
			return errTooBig // check before reserving: it allocates
		}
		total := 4 + int(size)
		if err := c.ReserveInbound(total); err != nil {
			return err
		}
		chunk := c.PeekChunk()
		if len(chunk) < total {
			return nil // the rest is on its way
		}
		handle(chunk[4:total])
		c.Discard(total)
	}
}
```

**Encode in place.** Inside a stream connection's own callback on the native
backend, `ReserveOutbound` hands you room in the send queue to encode a reply
into. Anywhere else it returns an error; encode into a pooled buffer and give
it away with `WriteOwned` instead:

```go
if dst, err := c.ReserveOutbound(n); err == nil {
	encode(dst) // fill every byte before the callback returns or flushes
} else {
	buf := uio.AcquireBuffer(n)
	encode(buf.AvailableBuffer()[:n])
	buf.CommitWrite(n)
	if _, err := c.WriteOwned(buf); err == uio.ErrOutboundOverflow {
		uio.ReleaseBuffer(buf) // overflow is the only error that hands it back
	}
}
```

## Rules of the road

- The inbound methods — `Read`, `WriteTo`, `Peek`, `PeekChunk`, `Discard`,
  `InboundBuffered` and `ReserveInbound` — may only be called inside the
  connection's own callback. Slices they return are valid until the callback
  returns or consumes more input; copy anything you hand to another goroutine.
  Builds with `-race` catch most misuse with a panic.
- `Write`, `Writev`, `WriteString`, `WriteOwned`, `Flush`, `Close` and
  `CloseWith` are safe from any goroutine.
- `OnOutbound` can run concurrently with the connection's other callbacks.
- `Events.Close` only requests shutdown and returns at once; `Serve` returns
  once everything has stopped, or call `Events.Wait` from another goroutine.
- `Dial` blocks the goroutine that calls it. From a callback, prefer dialing on
  a goroutine of your own.
- On the `stdio` backend, `Executor` is not used and `OnOpen` runs on an
  event-loop goroutine: keep it short, and do not `Dial` from it (that returns
  `ErrDialOnEventLoop`).

The [API reference](https://pkg.go.dev/github.com/urpc/uio) has the full
contract of every method.

## License

[Apache License 2.0](LICENSE)

The Go gopher in the banner is based on the original design by
[Renée French](https://reneefrench.blogspot.com/), licensed under
[CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).
