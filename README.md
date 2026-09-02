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

	// Addrs is the listening addr list for a server.
	Addrs []string

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

## License
The repository released under version 2.0 of the Apache License.
