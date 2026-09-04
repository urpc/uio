# UIO - Ultra Fast I/O Framework for Go

[![GoDoc][1]][2] [![license-Apache 2][3]][4]

[1]: https://godoc.org/github.com/urpc/uio?status.svg
[2]: https://godoc.org/github.com/urpc/uio
[3]: https://img.shields.io/badge/license-Apache%202-blue.svg
[4]: LICENSE

**UIO** (pronounced "ultra-IO") is a high-performance, event-driven networking framework for Go, designed for building scalable and efficient TCP/UDP servers and clients. It leverages modern I/O multiplexing techniques and provides a lightweight, non-blocking API for low-latency applications.

## Features

- 🚀 **Event-Driven Architecture**: Built on epoll/kqueue (Unix-like) for optimal I/O scheduling and `stdio` for others platform (Windows).
- 🌐 **Cross-Platform**: Supports Linux, macOS, BSD variants, and Windows.
- 🔄 **Protocol Support**: TCP, TCP4/TCP6, UDP, UDP4/UDP6, Unix domain sockets.
- ⚡  **Zero-Copy Optimizations**: Batched read/write operations minimize memory copies.
- 🧩 **Flexible Event Hooks**: `OnOpen`, `OnData`, `OnClose` callbacks for connection lifecycle management.
- 🔧 **Tunable Parameters**:
    - Custom buffer sizes
    - Write buffering thresholds
    - Per-connection inbound/outbound limits
    - SO_REUSEPORT support

## Installation

```bash
go get github.com/urpc/uio
```

## Overview

```go

type Events struct {
	// Pollers is set up to start the given number of event-loop goroutine.
	// The default value is runtime.NumCPU().
	Pollers int

	// ReusePort indicates whether to set up the SO_REUSEPORT socket option.
	// The default value is false.
	ReusePort bool

	// LockOSThread is used to determine whether each I/O event-loop is associated to an OS thread.
	// The default value is false.
	LockOSThread bool

	// MaxBufferSize is the maximum number of bytes that can be read from the remote when the readable event comes.
	// The default value is 4KB.
	MaxBufferSize int

	// WriteBufferedThreshold enabled when value is greater than 0, writes will go into the outbound buffer instead of attempting to send them out immediately,
	// unless the outbound buffer reaches the threshold or the flush function is manually called.
	//
	// If you have multiple Write call requirements, opening it will improve write performance because it reduces the number of system calls by merging multiple write operations to improve performance.
	// The default value is 0.
	WriteBufferedThreshold int

	// MaxOutboundBuffered limits accepted but unsent payload bytes per
	// connection. Zero disables the limit.
	MaxOutboundBuffered int

	// MaxPendingWrites limits write tasks that have not yet been consumed by
	// the connection's event loop. Values <= 0 use the default of 1024.
	MaxPendingWrites int

	// MaxInboundBuffered limits unread payload retained per connection. Zero
	// disables the limit.
	MaxInboundBuffered int

	// OnOpen fires when a new connection has been opened.
	OnOpen func(c Conn)

	// OnData fires when a socket receives data from the remote.
	OnData func(c Conn) error

	// OnClose fires when a connection has been closed.
	OnClose func(c Conn, err error)

	// OnInbound when any bytes read by a socket, it triggers the inbound event.
	OnInbound func(c Conn, readBytes int)

	// OnOutbound when any bytes write to a socket, it triggers the outbound event.
	OnOutbound func(c Conn, writeBytes int)

	// OnStart it triggers on the server initialized.
	OnStart func(ev *Events)

	// OnStop it triggers on the server closed.
	OnStop func(ev *Events)
}
```

## Quick Start

Basic Echo Server

`Events.Dial` and `Events.DialContext` perform synchronous resolution and
connection setup. Calls from callbacks currently running on an event loop
return `ErrDialOnEventLoop`; start the call from an external goroutine instead.
Use `DialContext` when the operation needs cancellation or a deadline.
`Events.Serve` accepts zero or one listening address. Call `Serve()` without an
address when using an Events instance only for outbound dialing.

`Events.Adopt` transfers an already-established stream connection into the
event loops. The `Events` instance must already be serving, and ownership is
consumed on both success and failure; the caller must never use the original
`net.Conn` again after calling it.

Inside a connection callback, `Conn.PeekChunk` exposes the first contiguous
inbound chunk without copying it. Process the returned slice before calling
`Discard`; the slice is invalid after `Discard` or after the callback returns.

For encoders that can write into caller-provided storage, `AcquireBuffer` and
`Conn.WriteOwned` avoid copying the encoded result into asynchronous outbound
storage. `WriteOwned` consumes the buffer on both success and failure:

```go
buffer := uio.AcquireBuffer(size)
dst := buffer.AvailableBuffer()[:size]
n, err := encode(dst)
if err != nil {
	uio.ReleaseBuffer(buffer)
	return err
}
buffer.CommitWrite(n)
_, err = conn.WriteOwned(buffer)
```

```go
package main

import (
	"fmt"

	"github.com/urpc/uio"
)

func main() {

	var events uio.Events

	events.OnOpen = func(c uio.Conn) {
		fmt.Println("connection opened:", c.RemoteAddr())
	}

	events.OnData = func(c uio.Conn) error {
		_, err := c.WriteTo(c)
		return err
	}

	events.OnClose = func(c uio.Conn, err error) {
		fmt.Println("connection closed:", c.RemoteAddr())
	}

	if err := events.Serve(":9527"); nil != err {
		fmt.Println("server exited with error:", err)
	}
}

```

## tcpkali2 Benchmark (2026-09-05)

The following results use [tcpkali2 0.3.0](https://github.com/limpo1989/tcpkali2)
with 1,000 connections, a 3-second
warmup, a 10-second measurement window, 1 KiB random messages, `--pipeline`,
and the default TCP_NODELAY setting. Every run completed with a 100% success
rate and zero connection errors. The UWS server used here has compression
disabled. P99 is saturated pipeline latency, not idle single-request latency.

Plain TCP was run with:

```bash
tcpkali2 -c 1000 --connect-rate 2000 -T 10s --warmup 3s \
  -s 1024 --pipeline 127.0.0.1:9527
```

The UWS run adds `--websocket` and targets port `19701`.

| OS | Service | Backend | Requests/s | Avg latency | P99 latency | Bandwidth |
| --- | --- | --- | ---: | ---: | ---: | ---: |
| macOS M4 Pro | UIO TCP echo | events | 1,060,782 | 30.22 ms | 106.05 ms | 2,172 MB/s |
| macOS M4 Pro | UIO TCP echo | stdio | 2,279,489 | 13.07 ms | 22.69 ms | 4,668 MB/s |
| macOS M4 Pro | UWS echo | events | 651,341 | 48.61 ms | 479.49 ms | 1,334 MB/s |
| macOS M4 Pro | UWS echo | stdio | 2,020,297 | 14.09 ms | 27.57 ms | 4,138 MB/s |
| Linux Xeon, 48 logical CPUs | UIO TCP echo | events | 3,726,609 | 5.77 ms | 12.19 ms | 7,632 MB/s |
| Linux Xeon, 48 logical CPUs | UIO TCP echo | stdio | 3,370,873 | 6.37 ms | 13.62 ms | 6,904 MB/s |
| Linux Xeon, 48 logical CPUs | UWS echo | events | 946,234 | 32.60 ms | 81.66 ms | 1,938 MB/s |
| Linux Xeon, 48 logical CPUs | UWS echo | stdio | 2,004,953 | 10.69 ms | 22.32 ms | 4,106 MB/s |

Linux peak RSS sampled during the same runs was approximately 10 MiB for UIO
events, 62 MiB for UIO stdio, 13 MiB for UWS events, and 66 MiB for UWS stdio.
The difference comes from stdio's per-connection read/write goroutines and
buffers; it is expected to grow with the number of live connections.

### Selection guidance

- For UWS or high-throughput RPC with a moderate number of active connections,
  stdio is currently the faster path and has substantially lower saturated P99.
- For plain UIO TCP echo on the tested Linux host, events is about 11% faster
  and uses much less memory. It is the better choice when connection count is
  the primary constraint.
- For large numbers of long-lived connections, prefer events even when stdio
  wins throughput: its memory footprint is shared by event loops instead of
  being proportional to two goroutines and a read buffer per connection.
- Results are platform and workload dependent. Linux and macOS use different
  pollers and CPU/NUMA topologies, and pipeline saturation amplifies queueing
  latency. Use the same connection count, payload, Go version, CPU placement,
  and warmup when comparing a deployment.

## License

The repository released under version 2.0 of the Apache License.
