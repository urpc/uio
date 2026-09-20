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
	// The default value is 4, capped by runtime.NumCPU().
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
`Events.Serve` listens on every supplied address. Call `Serve()` without an
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

## tcpkali2 Benchmark (2026-09-20)

The following results use [tcpkali2 0.4.0](https://github.com/limpo1989/tcpkali2)
on a dual-socket Xeon E5-2690 v3 host with 48 logical CPUs. Server and client
CPU resources were isolated with `taskset`: the server used all 24 logical
CPUs in NUMA node 0 (`0-11,24-35`) and tcpkali2 used all 24 logical CPUs in
NUMA node 1 (`12-23,36-47`). Both events and stdio ran with `GOMAXPROCS=24`;
events additionally used 24 pollers, while tcpkali2 used `-w 24`.

The load used 1,000 loopback connections, a 3-second warmup, a 10-second
measurement window, 1 KiB random messages, `--pipeline`, and the default
TCP_NODELAY setting. Each table row is the run with the median request rate
from three runs. Every run completed with a 100% success rate and zero
connection errors. The UWS server had compression disabled. P99 is saturated
pipeline latency, not idle single-request latency. UIO revision `cfee206` was
built with Go 1.27.1 on Ubuntu 22.04.5 with Linux 6.8.0.

These numbers are not directly comparable with the previous tcpkali2 0.3.0
table. tcpkali2 v0.4.0 uses larger pipeline batches and batched statistics,
and the previous table did not isolate equal server/client CPU budgets.

Plain TCP was run with:

```bash
GOMAXPROCS=24 taskset -c 0-11,24-35 ./server

taskset -c 12-23,36-47 tcpkali2 -w 24 -c 1000 \
  --connect-rate 2000 -T 10s --warmup 3s -s 1024 \
  --pipeline 127.0.0.1:9527
```

The events server sets `Pollers` to 24. Both backends were tested with the
default 4 KiB `MaxBufferSize` and a custom 16 KiB value. The UWS run adds
`--websocket` and targets port `19701`.

| Service | Backend | MaxBufferSize | Server CPUs | Requests/s | Avg latency | P99 latency | Bandwidth | Peak RSS |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| UIO TCP echo | events | 4 KiB | 24 | 5,555,553 | 5.74 ms | 14.05 ms | 11,378 MB/s | 7.5 MiB |
| UIO TCP echo | events | 16 KiB | 24 | 9,946,995 | 2.38 ms | 7.04 ms | 20,371 MB/s | 8.0 MiB |
| UIO TCP echo | stdio | 4 KiB | 24 | 8,938,151 | 3.53 ms | 10.66 ms | 18,305 MB/s | 56.2 MiB |
| UIO TCP echo | stdio | 16 KiB | 24 | 9,652,034 | 3.24 ms | 9.68 ms | 19,767 MB/s | 84.5 MiB |
| UWS echo | events | 4 KiB | 24 | 5,520,194 | 4.16 ms | 13.67 ms | 11,305 MB/s | 32.4 MiB |
| UWS echo | events | 16 KiB | 24 | 5,486,905 | 4.18 ms | 14.01 ms | 11,237 MB/s | 32.3 MiB |
| UWS echo | stdio | 4 KiB | 24 | 5,377,469 | 4.33 ms | 14.24 ms | 11,013 MB/s | 81.0 MiB |
| UWS echo | stdio | 16 KiB | 24 | 5,388,815 | 4.27 ms | 14.34 ms | 11,036 MB/s | 105.6 MiB |

Bandwidth is aggregate application traffic in both directions. Peak RSS was
sampled every 100 ms during the selected median-throughput run. stdio's
per-connection read/write goroutines and buffers account for most of its
higher memory use and scale with the number of live connections.

### Selection guidance

- `MaxBufferSize` materially affects plain TCP batching. Raising events from
  4 KiB to 16 KiB improved TCP echo throughput by about 79%, while stdio
  improved by about 8%. With both tuned to 16 KiB, events was about 3% faster,
  had lower latency, and used roughly one tenth of stdio's RSS.
- For UWS, events is about 2.7% faster than stdio in this test, with slightly
  lower latency and roughly 60% lower RSS at 4 KiB. Raising `MaxBufferSize` to
  16 KiB did not materially improve either backend for the 1 KiB WebSocket
  workload, and increased stdio RSS by about 30%, so 4 KiB remains the better
  choice for this profile.
- Tune `MaxBufferSize` against the application protocol and payload. Larger
  reads reduce syscall and callback overhead for streaming TCP, but they are
  not a universal throughput improvement.
- For large numbers of long-lived or mostly idle connections, prefer events:
  its memory footprint is shared by event loops instead of being proportional
  to two goroutines and a read buffer per connection.
- Results remain workload dependent. Pipeline saturation amplifies queueing
  latency, and placing server and client on separate NUMA nodes adds inter-node
  loopback traffic. Use the same client version, connection count, payload,
  Go version, poller/worker counts, CPU placement, and warmup when comparing a
  deployment.

## License

The repository released under version 2.0 of the Apache License.
