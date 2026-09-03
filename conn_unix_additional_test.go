//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/fdmap"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
	"github.com/urpc/uio/internal/taskqueue"
	"golang.org/x/sys/unix"
)

func newPartialStreamWriter(t *testing.T) (writer, reader int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.SetNonblock(fds[0], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		t.Fatal(err)
	}
	if err = unix.SetNonblock(fds[1], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		t.Fatal(err)
	}
	if err = unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 4096); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		t.Fatal(err)
	}
	return fds[0], fds[1]
}

func readStreamToEOF(t *testing.T, fd int) []byte {
	t.Helper()
	var result []byte
	buffer := make([]byte, 64*1024)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, err := unix.Read(fd, buffer)
		if n > 0 {
			result = append(result, buffer[:n]...)
		}
		if n == 0 && err == nil {
			return result
		}
		if err == nil {
			continue
		}
		if isWouldBlock(err) {
			time.Sleep(time.Millisecond)
			continue
		}
		t.Fatal(err)
	}
	t.Fatal("stream did not reach EOF")
	return nil
}

func TestUDPPeerKey(t *testing.T) {
	key4 := socket.UDPAddress{Family: 4, Port: 1234}
	copy(key4.Addr[:4], []byte{127, 0, 0, 1})
	if duplicate := key4; duplicate != key4 {
		t.Fatalf("duplicate IPv4 key = %#v", duplicate)
	}
	key6 := socket.UDPAddress{Family: 6, Port: 1234, Zone: 2}
	key6.Addr[15] = 1
	if key6 == key4 {
		t.Fatalf("IPv6 key = %#v matched IPv4", key6)
	}
	otherZone := key6
	otherZone.Zone++
	if otherZone == key6 {
		t.Fatal("IPv6 keys ignored zone")
	}
}

func TestExistingUDPPeerPacketDoesNotAllocate(t *testing.T) {
	var opened, packets int
	events := &Events{
		OnOpen: func(Conn) { opened++ },
		OnData: func(Conn) error {
			packets++
			return nil
		},
	}
	loop := &eventLoop{}
	loop.loopGoid.Store(currentGoroutineID())
	server := &fdConn{fd: -1, udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)}}
	server.events, server.loop = events, loop
	source := socket.UDPAddress{Family: 4, Port: 1234}
	copy(source.Addr[:4], []byte{127, 0, 0, 1})
	server.handleUDPPacket([]byte("first"), source)
	if opened != 1 || packets != 1 || len(server.udp.peers) != 1 {
		t.Fatalf("first packet created %d peers and delivered %d packets", opened, packets)
	}
	next := []byte("next")
	allocs := testing.AllocsPerRun(1000, func() {
		server.handleUDPPacket(next, source)
	})
	if allocs != 0 {
		t.Fatalf("existing UDP peer packet allocated %.2f objects/op", allocs)
	}
	if opened != 1 || packets != 1002 || len(server.udp.peers) != 1 {
		t.Fatalf("steady packets created %d peers and delivered %d packets", opened, packets)
	}
}

func newUDP4TestPair(t *testing.T) (receiver, sender int, target *unix.SockaddrInet4) {
	t.Helper()
	receiver, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Bind(receiver, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = unix.Close(receiver)
		t.Fatal(err)
	}
	addr, err := unix.Getsockname(receiver)
	if err != nil {
		_ = unix.Close(receiver)
		t.Fatal(err)
	}
	target = addr.(*unix.SockaddrInet4)
	sender, err = unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		_ = unix.Close(receiver)
		t.Fatal(err)
	}
	if err = unix.SetNonblock(receiver, true); err != nil {
		_ = unix.Close(receiver)
		_ = unix.Close(sender)
		t.Fatal(err)
	}
	return receiver, sender, target
}

func TestExistingUDPPeerReceiveDoesNotAllocate(t *testing.T) {
	receiver, sender, target := newUDP4TestPair(t)
	defer unix.Close(receiver)
	defer unix.Close(sender)
	packets := 0
	events := &Events{OnData: func(Conn) error {
		packets++
		return nil
	}}
	loop := &eventLoop{buffer: make([]byte, 64)}
	loop.loopGoid.Store(currentGoroutineID())
	server := &fdConn{
		fd: receiver, udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)}, interest: poller.Readable,
	}
	server.events, server.loop = events, loop
	payload := []byte{'x'}
	sendAndReceive := func() {
		before := packets
		if err := unix.Sendto(sender, payload, 0, target); err != nil {
			panic(err)
		}
		for polls := 0; packets == before; polls++ {
			if polls == 10000 {
				panic("sent UDP packet was not consumed")
			}
			if err := server.onRecvUDP(); err != nil {
				panic(err)
			}
			if packets == before {
				runtime.Gosched()
			}
		}
		if packets != before+1 {
			panic("UDP receive consumed more than one packet")
		}
	}
	sendAndReceive() // create the peer before measuring the steady path
	const measuredPackets = 1000
	allocs := testing.AllocsPerRun(measuredPackets, sendAndReceive)
	if allocs != 0 {
		t.Fatalf("full existing-peer receive path allocated %.2f objects/packet", allocs)
	}
	if packets != measuredPackets+2 || len(server.udp.peers) != 1 {
		t.Fatalf("received %d packets across %d peers", packets, len(server.udp.peers))
	}
}

func TestUDPReadEventDoesNotExceedPacketBudget(t *testing.T) {
	receiver, sender, target := newUDP4TestPair(t)
	defer unix.Close(receiver)
	defer unix.Close(sender)
	packets := 0
	events := &Events{OnData: func(Conn) error {
		packets++
		return nil
	}}
	loop := &eventLoop{buffer: make([]byte, 64)}
	loop.loopGoid.Store(currentGoroutineID())
	conn := &fdConn{fd: receiver, udp: &unixUDPState{}, interest: poller.Readable}
	conn.events, conn.loop = events, loop
	for range 32 {
		if err := unix.Sendto(sender, []byte{'x'}, 0, target); err != nil {
			t.Fatal(err)
		}
	}
	before := packets
	if err := conn.onRecvUDP(); err != nil {
		t.Fatal(err)
	}
	if handled := packets - before; handled > 16 {
		t.Fatalf("first read event handled %d packets, limit 16", handled)
	}
	before = packets
	if err := conn.onRecvUDP(); err != nil {
		t.Fatal(err)
	}
	if handled := packets - before; handled > 16 {
		t.Fatalf("second read event handled %d packets, limit 16", handled)
	}
}

func fillDatagramSendBuffer(t *testing.T, fd int) []byte {
	t.Helper()
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 4096); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1024)
	for range 1 << 16 {
		n, err := unix.Write(fd, payload)
		if isUDPSendBlocked(err) {
			return payload
		}
		if err != nil || n != len(payload) {
			t.Fatalf("fill datagram socket = %d, %v", n, err)
		}
	}
	t.Fatal("datagram socket did not reach EAGAIN")
	return nil
}

func TestUDPWouldBlockReportsUnflushedDatagram(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	payload := fillDatagramSendBuffer(t, fds[0])

	direct := &fdConn{fd: fds[0], udp: &unixUDPState{}}
	direct.events = &Events{}
	direct.loop = &eventLoop{}
	direct.loop.loopGoid.Store(currentGoroutineID())
	directBuffer := AcquireBuffer(len(payload))
	_, _ = directBuffer.Write(payload)
	if n, writeErr := direct.WriteOwned(directBuffer); n != 0 || !isUDPSendBlocked(writeErr) {
		t.Fatalf("direct UDP WriteOwned = %d, %v", n, writeErr)
	}
	if direct.isClosing() {
		t.Fatal("direct EAGAIN closed a datagram that was never accepted")
	}

	closed := make(chan error, 1)
	events := &Events{MaxPendingWrites: 1, OnClose: func(_ Conn, errorValue error) { closed <- errorValue }}
	loop := &eventLoop{tasks: taskqueue.New[*task]()}
	loop.wakePending.Store(true)
	server := &fdConn{udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)}}
	conn := &fdConn{fd: fds[0], udp: &unixUDPState{server: server}}
	conn.events, conn.loop = events, loop
	server.udp.peers[conn.udp.key] = conn
	otherKey := socket.UDPAddress{Family: 4, Port: 1}
	other := &fdConn{fd: fds[0], udp: &unixUDPState{server: server, key: otherKey}}
	server.udp.peers[otherKey] = other
	queuedBuffer := AcquireBuffer(len(payload))
	_, _ = queuedBuffer.Write(payload)
	if n, writeErr := conn.WriteOwned(queuedBuffer); n != len(payload) || writeErr != nil {
		t.Fatalf("queued UDP WriteOwned = %d, %v", n, writeErr)
	}
	writeNode := loop.tasks.Drain()
	if writeNode == nil || writeNode.TakeNext() != nil {
		t.Fatal("UDP WriteOwned did not enqueue exactly one write task")
	}
	loop.runTask(writeNode.Value)
	if conn.pending.Load() != int64(len(payload)) || conn.queuedWrites.Load() != 0 {
		t.Fatalf("EAGAIN counters = pending %d, queued %d", conn.pending.Load(), conn.queuedWrites.Load())
	}
	closeNode := loop.tasks.Drain()
	if closeNode == nil || closeNode.TakeNext() != nil {
		t.Fatal("UDP EAGAIN did not enqueue exactly one close task")
	}
	loop.runTask(closeNode.Value)
	closeErr := <-closed
	if (!errors.Is(closeErr, unix.EAGAIN) && !errors.Is(closeErr, unix.ENOBUFS)) || !errors.Is(closeErr, ErrUnflushedData) {
		t.Fatalf("UDP close error = %v", closeErr)
	}
	var unflushed UnflushedError
	if !errors.As(closeErr, &unflushed) || unflushed.Remaining != int64(len(payload)) {
		t.Fatalf("UDP unflushed error = %#v", unflushed)
	}
	if conn.pending.Load() != 0 || len(server.udp.peers) != 1 || server.udp.peers[otherKey] != other {
		t.Fatalf("closed UDP state = pending %d, peers %d", conn.pending.Load(), len(server.udp.peers))
	}
}

func releaseTestTasks(loop *eventLoop) {
	for node := loop.tasks.Drain(); node != nil; {
		next := node.TakeNext()
		releaseTask(node.Value)
		node = next
	}
}

func TestUnixSocketOptions(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	loop := &eventLoop{}
	loop.loopGoid.Store(currentGoroutineID())
	conn := &fdConn{fd: fd}
	conn.events = &Events{}
	conn.loop = loop
	for name, set := range map[string]func() error{
		"linger":            func() error { return conn.SetLinger(0) },
		"no delay":          func() error { return conn.SetNoDelay(false) },
		"read buffer":       func() error { return conn.SetReadBuffer(4096) },
		"write buffer":      func() error { return conn.SetWriteBuffer(4096) },
		"keep alive":        func() error { return conn.SetKeepAlive(true) },
		"keep alive period": func() error { return conn.SetKeepAlivePeriod(1) },
	} {
		t.Run(name, func(t *testing.T) {
			loop.loopGoid.Store(currentGoroutineID())
			if err := set(); err != nil {
				t.Fatalf("socket option failed: %v", err)
			}
		})
	}
	if boolInt(true) != 1 || boolInt(false) != 0 {
		t.Fatal("boolInt returned an invalid value")
	}
	conn.closing.Store(true)
	if err := conn.SetNoDelay(true); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed SetNoDelay error = %v", err)
	}
	conn.closed = true
	if err := conn.applySocketOption(optionLinger, 0); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed applySocketOption error = %v", err)
	}
}

func TestCloseUnregisteredDescriptorOwnership(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	conn := &fdConn{fd: fds[0]}
	conn.closeUnregistered()
	conn.closeUnregistered()
	if _, err := unix.Write(fds[0], []byte("x")); !errors.Is(err, unix.EBADF) {
		t.Fatalf("write after close error = %v", err)
	}

	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writeFile.Close()
	conn = &fdConn{udp: &unixUDPState{file: readFile}}
	conn.closeUnregistered()
	if _, err := readFile.Stat(); err == nil {
		t.Fatal("owned file remained open")
	}
}

func TestLoopDirectWritevAndFlush(t *testing.T) {
	type writeResult struct {
		n   int
		err error
	}
	written := make(chan writeResult, 1)
	events := &Events{Pollers: 1}
	events.OnOpen = func(conn Conn) {
		if n, err := conn.Write(nil); n != 0 || err != nil {
			t.Errorf("empty Write = %d, %v", n, err)
		}
		if n, err := conn.Writev(nil); n != 0 || err != nil {
			t.Errorf("empty Writev = %d, %v", n, err)
		}
		n, err := conn.Writev([][]byte{[]byte("ab"), []byte("cd")})
		if flushErr := conn.Flush(); err == nil {
			err = flushErr
		}
		written <- writeResult{n: n, err: err}
	}
	testConn := newTestConnection(t, events)
	got := <-written
	if got.err != nil || got.n != 4 {
		t.Fatalf("Writev = %d, %v", got.n, got.err)
	}
	if got := string(readPeer(t, testConn.peer, 4)); got != "abcd" {
		t.Fatalf("peer received %q", got)
	}
}

func TestDirectPartialWriteOverflowAbortsStream(t *testing.T) {
	payload := bytes.Repeat([]byte{'P'}, 8<<20)
	tests := []struct {
		name  string
		write func(*fdConn, []byte) (int, error)
	}{
		{
			name: "Write",
			write: func(conn *fdConn, data []byte) (int, error) {
				return conn.writeOnLoop(data)
			},
		},
		{
			name: "Writev",
			write: func(conn *fdConn, data []byte) (int, error) {
				middle := len(data) / 2
				return conn.writevOnLoop([][]byte{data[:middle], data[middle:]}, len(data))
			},
		},
		{
			name: "WriteOwned",
			write: func(conn *fdConn, data []byte) (int, error) {
				owned := bytebuf.CloneBuffer(data)
				return conn.writeOwnedOnLoop(owned, len(data))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer, reader := newPartialStreamWriter(t)
			defer unix.Close(reader)
			writerOpen := true
			defer func() {
				if writerOpen {
					_ = unix.Close(writer)
				}
			}()

			closed := make(chan error, 1)
			events := &Events{
				MaxOutboundBuffered: 1,
				MaxPendingWrites:    2,
				OnClose: func(_ Conn, err error) {
					closed <- err
				},
			}
			if err := events.initConfig(); err != nil {
				t.Fatal(err)
			}
			loop, err := newEventLoop(events)
			if err != nil {
				t.Fatal(err)
			}
			defer loop.poller.Close(nil)
			conn := &fdConn{fd: writer}
			conn.events, conn.loop = events, loop

			// Model an external producer winning submitMu after the callback chose
			// the direct path but before its syscall returned.
			marker := bytebuf.CloneBuffer([]byte{'M'})
			if n, queueErr := conn.queueOwnedWrite(marker, 1); n != 1 || queueErr != nil {
				t.Fatalf("queue marker = %d, %v", n, queueErr)
			}

			written, writeErr := test.write(conn, payload)
			if !errors.Is(writeErr, ErrOutboundOverflow) {
				t.Fatalf("partial write error = %v", writeErr)
			}
			if written <= 0 || written >= len(payload) {
				t.Fatalf("write result = %d, want a partial write of %d bytes", written, len(payload))
			}
			if !conn.writeFailed || !conn.isClosing() {
				t.Fatalf("failed stream state = writeFailed %v, closing %v", conn.writeFailed, conn.isClosing())
			}

			writeNode := loop.tasks.Drain()
			if writeNode == nil || writeNode.Value.kind != writeTask {
				t.Fatal("queued write task is missing")
			}
			closeNode := writeNode.TakeNext()
			if closeNode == nil || closeNode.Value.kind != closeTask || closeNode.TakeNext() != nil {
				t.Fatal("close task did not follow the queued write")
			}
			loop.runTask(writeNode.Value)
			if conn.pending.Load() != 1 || !conn.outbound.Empty() {
				t.Fatalf("discarded task left pending %d, outbound %d", conn.pending.Load(), conn.outbound.Len())
			}
			// A failed stream must not flush even if an earlier bug or callback
			// leaves bytes in outbound before the close task runs.
			_, _ = conn.outbound.Write([]byte{'N'})
			conn.pending.Add(1)
			loop.runTask(closeNode.Value)
			writerOpen = false
			closeErr := <-closed
			var unflushed UnflushedError
			if !errors.Is(closeErr, ErrOutboundOverflow) || !errors.As(closeErr, &unflushed) || unflushed.Remaining != 2 {
				t.Fatalf("close error = %v", closeErr)
			}

			received := readStreamToEOF(t, reader)
			if bytes.Contains(received, []byte{'M'}) || bytes.Contains(received, []byte{'N'}) {
				t.Fatal("bytes queued after the partial frame reached the peer")
			}
			if len(received) != written || !bytes.Equal(received, payload[:written]) {
				t.Fatalf("peer received %d bytes, want only the %d-byte direct prefix", len(received), written)
			}
			if conn.pending.Load() != 0 {
				t.Fatalf("close left %d pending bytes", conn.pending.Load())
			}
		})
	}
}

func TestLoopBufferedWritevAndOverflow(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	loop := &eventLoop{}
	loop.loopGoid.Store(currentGoroutineID())
	conn := &fdConn{fd: fds[0]}
	conn.events = &Events{WriteBufferedThreshold: 16, MaxOutboundBuffered: 8}
	conn.loop = loop
	conn.interest = poller.Readable
	if n, err := conn.Writev([][]byte{[]byte("a"), []byte("b")}); err != nil || n != 2 {
		t.Fatalf("buffered Writev = %d, %v", n, err)
	}
	if conn.OutboundBuffered() != 2 {
		t.Fatalf("buffered bytes = %d", conn.OutboundBuffered())
	}
	conn.writeBlocked = true
	if n, err := conn.flushOnLoop(); err != nil || n != 0 {
		t.Fatalf("blocked flush = %d, %v", n, err)
	}
	conn.writeBlocked = false
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, fds[1], 2)); got != "ab" {
		t.Fatalf("peer received %q", got)
	}
	conn.events.MaxOutboundBuffered = 1
	if n, err := conn.Writev([][]byte{[]byte("a"), []byte("b")}); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("overflow Writev = %d, %v", n, err)
	}
	if n, err := conn.Write([]byte("ab")); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("overflow Write = %d, %v", n, err)
	}
	conn.events.MaxOutboundBuffered = 8
	conn.writeBlocked = true
	if err := conn.fireWriteEvent(); err != nil || conn.writeBlocked {
		t.Fatalf("fireWriteEvent = %v, blocked %v", err, conn.writeBlocked)
	}
	conn.udp = &unixUDPState{}
	if n, err := conn.Writev([][]byte{[]byte("x")}); n != 0 || !errors.Is(err, errUnsupported) {
		t.Fatalf("UDP Writev = %d, %v", n, err)
	}
	if err := conn.fireWriteEvent(); err != nil {
		t.Fatalf("UDP fireWriteEvent = %v", err)
	}
}

func TestLoopWriteOwnedTransfersBufferWithoutCopy(t *testing.T) {
	loop := &eventLoop{}
	loop.loopGoid.Store(currentGoroutineID())
	conn := &fdConn{fd: -1}
	conn.events = &Events{WriteBufferedThreshold: 16, MaxOutboundBuffered: 16}
	conn.loop = loop

	buffer := AcquireBuffer(8)
	dst := buffer.AvailableBuffer()[:8]
	encoded := &dst[0]
	buffer.CommitWrite(copy(dst, "payload"))
	n, err := conn.WriteOwned(buffer)
	if err != nil || n != 7 {
		t.Fatalf("WriteOwned = %d, %v", n, err)
	}
	peeked := conn.outbound.Peek(make([]byte, 7))
	if string(peeked) != "payload" || &peeked[0] != encoded {
		t.Fatal("owned write was copied before entering outbound")
	}
	if conn.OutboundBuffered() != 7 {
		t.Fatalf("buffered bytes = %d, want 7", conn.OutboundBuffered())
	}
	conn.outbound.Reset()
	conn.pending.Store(0)
}

func TestExternalWriteOwnedTransfersBufferThroughTask(t *testing.T) {
	loop := &eventLoop{tasks: taskqueue.New[*task]()}
	loop.wakePending.Store(true)
	conn := &fdConn{fd: -1}
	conn.events = &Events{MaxPendingWrites: 1, MaxOutboundBuffered: 16}
	conn.loop = loop

	buffer := AcquireBuffer(8)
	dst := buffer.AvailableBuffer()[:8]
	encoded := &dst[0]
	buffer.CommitWrite(copy(dst, "payload"))
	n, err := conn.WriteOwned(buffer)
	if err != nil || n != 7 {
		t.Fatalf("WriteOwned = %d, %v", n, err)
	}
	batch := loop.tasks.Drain()
	if batch == nil || batch.Value.buf == nil {
		t.Fatal("owned write did not enqueue its buffer")
	}
	writeTask := batch.Value
	conn.runWriteTask(writeTask)
	releaseTask(writeTask)
	peeked := conn.outbound.Peek(make([]byte, 7))
	if string(peeked) != "payload" || &peeked[0] != encoded {
		t.Fatal("write task copied its owned buffer")
	}
	conn.outbound.Reset()
	conn.pending.Store(0)
}

func TestUnixWriteFailureAndRejectedQueues(t *testing.T) {
	loop := &eventLoop{tasks: taskqueue.New[*task]()}
	loop.loopGoid.Store(currentGoroutineID())
	loop.wakePending.Store(true)
	conn := &fdConn{fd: -1}
	conn.events = &Events{MaxPendingWrites: 1}
	conn.loop = loop
	if n, err := conn.Write([]byte("x")); n != 0 || err == nil {
		t.Fatalf("invalid-fd Write = %d, %v", n, err)
	}
	if !conn.isClosing() {
		t.Fatal("write failure did not request close")
	}
	releaseTestTasks(loop)

	loop = &eventLoop{tasks: taskqueue.New[*task]()}
	loop.loopGoid.Store(currentGoroutineID())
	loop.wakePending.Store(true)
	conn = &fdConn{fd: -1}
	conn.events = &Events{MaxPendingWrites: 1}
	conn.loop = loop
	if n, err := conn.Writev([][]byte{[]byte("x")}); n != 0 || err == nil {
		t.Fatalf("invalid-fd Writev = %d, %v", n, err)
	}
	if !conn.isClosing() {
		t.Fatal("writev failure did not request close")
	}
	releaseTestTasks(loop)

	loop = &eventLoop{tasks: taskqueue.New[*task]()}
	loop.wakePending.Store(true)
	conn = &fdConn{fd: -1, udp: &unixUDPState{}}
	conn.events = &Events{MaxPendingWrites: 1}
	conn.loop = loop
	conn.queuedWrites.Store(1)
	conn.pending.Store(1)
	write := acquireTask(writeTask, conn)
	write.buf = bytebuf.CloneBuffer([]byte("x"))
	conn.runWriteTask(write)
	if !conn.isClosing() || conn.pending.Load() != 1 {
		t.Fatal("UDP write task failure did not retain unflushed pending bytes")
	}
	releaseTask(write)
	releaseTestTasks(loop)

	loop = &eventLoop{tasks: taskqueue.New[*task]()}
	conn = &fdConn{closed: true}
	conn.events = &Events{}
	conn.loop = loop
	conn.queuedWrites.Store(1)
	conn.pending.Store(1)
	write = acquireTask(writeTask, conn)
	write.buf = bytebuf.CloneBuffer([]byte("x"))
	conn.runWriteTask(write)
	if conn.pending.Load() != 0 {
		t.Fatalf("closed write task left %d pending bytes", conn.pending.Load())
	}
	releaseTask(write)

	loop = &eventLoop{tasks: taskqueue.New[*task]()}
	stop := acquireTask(stopTask, nil)
	if !loop.tasks.Stop(&stop.node) {
		t.Fatal("failed to stop test queue")
	}
	conn = &fdConn{}
	conn.events = &Events{MaxPendingWrites: 1}
	conn.loop = loop
	if err := conn.Flush(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stopped queue Flush error = %v", err)
	}
	if n, err := conn.queueOwnedWrite(bytebuf.CloneBuffer([]byte("x")), 1); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stopped queue write = %d, %v", n, err)
	}
	releaseTestTasks(loop)
	conn.closed = true
	if err := conn.runFlushTask(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed runFlushTask error = %v", err)
	}

	conn = &fdConn{fd: -1}
	conn.events = &Events{}
	_, _ = conn.outbound.WriteString("x")
	conn.pending.Store(1)
	if n, err := conn.flushOnLoop(); n != 0 || err == nil {
		t.Fatalf("invalid-fd flush = %d, %v", n, err)
	}

	conn = &fdConn{}
	conn.events = &Events{MaxOutboundBuffered: 1}
	if n, err := conn.queueOwnedWrite(bytebuf.CloneBuffer([]byte("xx")), 2); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("oversize owned write = %d, %v", n, err)
	}
}

func TestRejectedExternalWriteDoesNotAllocate(t *testing.T) {
	payload := make([]byte, 65537)
	vec := [][]byte{payload}
	for _, test := range []struct {
		name  string
		setup func(*fdConn)
		write func(*fdConn) (int, error)
		want  error
	}{
		{
			name:  "Write/task_limit",
			setup: func(conn *fdConn) { conn.queuedWrites.Store(1) },
			write: func(conn *fdConn) (int, error) { return conn.Write(payload) },
			want:  ErrTaskQueueFull,
		},
		{
			name:  "Writev/task_limit",
			setup: func(conn *fdConn) { conn.queuedWrites.Store(1) },
			write: func(conn *fdConn) (int, error) { return conn.Writev(vec) },
			want:  ErrTaskQueueFull,
		},
		{
			name:  "Write/payload_limit",
			setup: func(conn *fdConn) { conn.pending.Store(1) },
			write: func(conn *fdConn) (int, error) { return conn.Write(payload) },
			want:  ErrOutboundOverflow,
		},
		{
			name:  "Writev/payload_limit",
			setup: func(conn *fdConn) { conn.pending.Store(1) },
			write: func(conn *fdConn) (int, error) { return conn.Writev(vec) },
			want:  ErrOutboundOverflow,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &Events{MaxOutboundBuffered: len(payload), MaxPendingWrites: 1}
			conn := &fdConn{}
			conn.events = events
			conn.loop = &eventLoop{}
			test.setup(conn)
			pending, queued := conn.pending.Load(), conn.queuedWrites.Load()
			var n int
			var err error
			allocs := testing.AllocsPerRun(100, func() {
				n, err = test.write(conn)
			})
			if allocs != 0 {
				t.Fatalf("rejected write allocated %.2f objects/op", allocs)
			}
			if n != 0 || !errors.Is(err, test.want) {
				t.Fatalf("rejected write = %d, %v", n, err)
			}
			if conn.pending.Load() != pending || conn.queuedWrites.Load() != queued {
				t.Fatalf("rejected write changed counters to pending %d, queued %d", conn.pending.Load(), conn.queuedWrites.Load())
			}
		})
	}
}

func TestUnixDeadlineAndClosedOperations(t *testing.T) {
	closed := make(chan error, 1)
	events := &Events{Pollers: 1, OnClose: func(_ Conn, err error) { closed <- err }}
	testConn := newTestConnection(t, events)
	if err := testConn.conn.SetReadBuffer(4096); err != nil {
		t.Fatal(err)
	}
	if err := testConn.conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := testConn.conn.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("write deadline error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write deadline did not close connection")
	}
	if err := testConn.conn.SetDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed deadline error = %v", err)
	}
	if err := testConn.conn.Flush(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Flush error = %v", err)
	}
	if err := testConn.conn.Wake(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Wake error = %v", err)
	}
}

func TestEventAndListenerHelperBranches(t *testing.T) {
	events := &Events{Pollers: 1, MaxBufferSize: 64}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	events.master = loop
	events.workers = []*eventLoop{loop}
	loop.loopGoid.Store(currentGoroutineID())
	if events.selectLoop(1) != loop || events.currentLoop() != loop {
		t.Fatal("current loop was not selected")
	}
	loop.loopGoid.Store(0)
	if (&Events{}).selectWorker(1) != nil {
		t.Fatal("selectWorker returned a loop from an empty set")
	}
	id := events.enterExternalCallback()
	if _, ok := events.callbackGoids.Load(id); !ok {
		t.Fatal("external callback was not registered")
	}
	events.leaveExternalCallback(id)
	if _, ok := events.callbackGoids.Load(id); ok {
		t.Fatal("external callback was not removed")
	}

	conn := &fdConn{}
	conn.events = events
	conn.inboundTail = []byte("discard")
	if err := events.onData(conn); err != nil || conn.InboundBuffered() != 0 {
		t.Fatalf("default OnData = %v, buffered %d", err, conn.InboundBuffered())
	}
	wantErr := errors.New("handler")
	events.OnData = func(Conn) error { return wantErr }
	if err := events.onData(conn); !errors.Is(err, wantErr) {
		t.Fatalf("OnData error = %v", err)
	}
	var inbound, outbound int
	events.OnInbound = func(Conn, int) { inbound++ }
	events.OnOutbound = func(Conn, int) { outbound++ }
	events.onSocketBytesRead(conn, 0)
	events.onSocketBytesRead(conn, 1)
	events.onSocketBytesWrite(conn, 0)
	events.onSocketBytesWrite(conn, 1)
	if inbound != 1 || outbound != 1 {
		t.Fatalf("byte callbacks = %d, %d", inbound, outbound)
	}

	missing := &fdConn{fd: -1}
	missing.events = events
	if err := events.addConn(missing); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("addConn without loop error = %v", err)
	}
	rejected := &fdConn{fd: -1}
	rejected.events = events
	if events.submitAccepted(rejected) {
		t.Fatal("submitAccepted accepted a connection without a loop")
	}

	ld := &acceptor{loop: loop, events: &Events{}, listeners: make(map[int]*listener)}
	ld.OnEvent(nil, 12345, poller.ReadEvents|poller.WriteEvents)
	ld.OnClose(nil, nil)
	if err := ld.onReadUDP(&listener{fd: 12345}); err == nil {
		t.Fatal("missing UDP server was accepted")
	}
	badEvents := &Events{}
	bad := &acceptor{events: badEvents, listeners: map[int]*listener{-1: {fd: -1}}}
	bad.OnEvent(nil, -1, poller.ReadEvents)
	if !badEvents.closing.Load() {
		t.Fatal("accept error did not close Events")
	}
}

func TestListenerAddressVariants(t *testing.T) {
	ld := &acceptor{}
	for _, address := range []string{"127.0.0.1:0", "udp://127.0.0.1:0"} {
		listener, err := ld.listen(address, false)
		if err != nil {
			t.Fatalf("listen %q: %v", address, err)
		}
		ld.closeListener(listener)
	}

	path := filepath.Join(t.TempDir(), "uio.sock")
	listener, err := ld.listen("unix://"+path, false)
	if err != nil {
		t.Fatal(err)
	}
	ld.closeListener(listener)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unix socket still exists: %v", err)
	}

	for _, address := range []string{"unsupported://address", "tcp://%", "tcp://bad-address"} {
		if listener, err := ld.listen(address, false); err == nil {
			ld.closeListener(listener)
			t.Fatalf("listen %q unexpectedly succeeded", address)
		}
	}

	listener, err = ld.listen("tcp://127.0.0.1:0", true)
	if err == nil {
		ld.closeListener(listener)
	}
}

func TestEventLoopInterestAndRegistrationErrors(t *testing.T) {
	events := &Events{Pollers: 1, MaxBufferSize: 64}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	conn := &fdConn{fd: fds[0]}
	conn.events = events
	conn.loop = loop
	if err := loop.poller.Add(conn.fd, poller.Readable); err != nil {
		t.Fatal(err)
	}
	conn.setInterest(poller.Readable)
	if err := loop.modRead(conn); err != nil {
		t.Fatal(err)
	}
	if err := loop.modWrite(conn); err != nil {
		t.Fatal(err)
	}
	if err := loop.modReadWrite(conn); err != nil {
		t.Fatal(err)
	}
	loop.OnEvent(nil, 12345, poller.ReadEvents)
	loop.OnClose(nil, nil)

	failedLoop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer failedLoop.poller.Close(nil)
	closedFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(closedFD); err != nil {
		t.Fatal(err)
	}
	failedConn := &fdConn{fd: closedFD}
	failedConn.events = events
	failedConn.loop = failedLoop
	if err := failedLoop.registerConn(failedConn); err == nil {
		t.Fatal("registerConn accepted a closed descriptor")
	}
}

func TestUnixDialFailureAndResourceCleanup(t *testing.T) {
	events := &Events{}
	if _, err := events.Dial("tcp://127.0.0.1:1", nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("unready Dial error = %v", err)
	}
	events.ready.Store(true)
	dialCtx, cancelDial := context.WithCancel(context.Background())
	cancelDial()
	if _, err := events.DialContext(dialCtx, "tcp://127.0.0.1:1", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled DialContext error = %v", err)
	}
	for _, address := range []string{"%", "unsupported://address", "tcp://127.0.0.1:0"} {
		if conn, err := events.Dial(address, nil); err == nil {
			_ = conn.Close()
			t.Fatalf("Dial %q unexpectedly succeeded", address)
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			err = conn.Close()
		}
		accepted <- err
	}()
	if conn, err := events.Dial("tcp://"+listener.Addr().String(), "context"); conn != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Dial without a worker = %v, %v", conn, err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}

	events.closing.Store(true)
	if _, err := events.Dial("tcp://127.0.0.1:1", nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closing Dial error = %v", err)
	}
}

func TestDialBeforeReadyDoesNotReadLoopState(t *testing.T) {
	events := &Events{}
	first, second := &eventLoop{}, &eventLoop{}
	firstWorkers := []*eventLoop{first}
	secondWorkers := []*eventLoop{second}
	started := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				events.master, events.workers = first, firstWorkers
				events.master, events.workers = second, secondWorkers
				runtime.Gosched()
			}
		}
	}()
	<-started
	for range 10000 {
		if _, err := events.Dial("tcp://127.0.0.1:1", nil); !errors.Is(err, net.ErrClosed) {
			close(stop)
			<-done
			t.Fatalf("Dial before ready error = %v", err)
		}
		runtime.Gosched()
	}
	close(stop)
	<-done
}

func TestStoppedLoopRejectsConnections(t *testing.T) {
	loop := &eventLoop{tasks: taskqueue.New[*task]()}
	stop := acquireTask(stopTask, nil)
	if !loop.tasks.Stop(&stop.node) {
		t.Fatal("failed to stop test loop")
	}
	events := &Events{}

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	conn := &fdConn{fd: fds[0]}
	conn.events = events
	conn.loop = loop
	if err := events.addConn(conn); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stopped addConn error = %v", err)
	}

	fds, err = unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	conn = &fdConn{fd: fds[0]}
	conn.events = events
	conn.loop = loop
	if events.submitAccepted(conn) {
		t.Fatal("stopped loop accepted a connection")
	}
	releaseTestTasks(loop)
}

func TestRegisterConnClosesOutOfRangeFD(t *testing.T) {
	oldMax := fdmap.MaxOpenFiles
	fdmap.MaxOpenFiles = 1
	mapping := fdmap.NewMap[fdConn]()
	fdmap.MaxOpenFiles = oldMax

	events := &Events{MaxBufferSize: 64}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	loop.fdMap = mapping
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	conn := &fdConn{fd: fds[0]}
	conn.events, conn.loop = events, loop
	if err = loop.registerConn(conn); !errors.Is(err, fdmap.ErrOutOfRange) {
		t.Fatalf("registerConn error = %v", err)
	}
	if _, err = unix.FcntlInt(uintptr(fds[0]), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("rejected descriptor was not closed: %v", err)
	}
}

func TestUnixClosedAndCallbackStateBranches(t *testing.T) {
	loop := &eventLoop{tasks: taskqueue.New[*task]()}
	loop.loopGoid.Store(currentGoroutineID())
	loop.wakePending.Store(true)
	events := &Events{MaxPendingWrites: 1}
	conn := &fdConn{fd: -1}
	conn.events = events
	conn.loop = loop

	conn.closing.Store(true)
	conn.inboundTail = []byte("discard")
	if err := conn.fireOnData(); err != nil || conn.InboundBuffered() != 0 {
		t.Fatalf("default fireOnData = %v, buffered %d", err, conn.InboundBuffered())
	}
	wantErr := errors.New("callback")
	events.OnData = func(Conn) error { return wantErr }
	if err := conn.fireOnData(); !errors.Is(err, wantErr) {
		t.Fatalf("callback fireOnData error = %v", err)
	}
	if err := conn.runWakeTask(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed runWakeTask error = %v", err)
	}
	if n, err := conn.Write([]byte("x")); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Write = %d, %v", n, err)
	}
	if n, err := conn.Writev([][]byte{[]byte("x")}); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Writev = %d, %v", n, err)
	}
	if n, err := conn.queueOwnedWrite(bytebuf.CloneBuffer([]byte("x")), 1); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closing queueOwnedWrite = %d, %v", n, err)
	}
	conn.submitTimeout(deadlineRead)
	events.closeConn(conn, wantErr)
	conn.closed = true
	conn.closeOnLoop(nil)

	conn.closing.Store(false)
	if err := conn.SetDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed in-loop SetDeadline error = %v", err)
	}

	stopped := &eventLoop{tasks: taskqueue.New[*task]()}
	stop := acquireTask(stopTask, nil)
	if !stopped.tasks.Stop(&stop.node) {
		t.Fatal("failed to stop task queue")
	}
	rejectedClose := &fdConn{fd: -1}
	rejectedClose.events = events
	rejectedClose.loop = stopped
	rejectedClose.requestClose(wantErr)
	if rejectedClose.deferredCloseErr == nil || !errors.Is(*rejectedClose.deferredCloseErr, wantErr) {
		t.Fatalf("deferred close error = %v", rejectedClose.deferredCloseErr)
	}

	rejectedTimeout := &fdConn{fd: -1}
	rejectedTimeout.events = events
	rejectedTimeout.loop = stopped
	rejectedTimeout.deadlines = &deadlineState{}
	rejectedTimeout.deadlines.readTimerGen.Store(3)
	rejectedTimeout.submitTimeout(deadlineRead)
	rejectedTimeout.deadlines.readGeneration = 3
	rejectedTimeout.deadlines.readDeadline = time.Now().Add(time.Hour)
	rejectedTimeout.handleTimeout(deadlineRead, 2)
	if rejectedTimeout.isClosing() {
		t.Fatal("stale timeout closed the connection")
	}
	releaseTestTasks(stopped)
}
