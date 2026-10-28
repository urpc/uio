//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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

package uio

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A graceful close lets a queue too large for the socket leave whole before
// the connection releases: writes are refused from the call, the peer reads
// everything, and OnClose reports the close's cause alone.
func TestCloseGracefullyDrainsQueuedOutput(t *testing.T) {
	closed := make(chan error, 1)
	events := &Events{Pollers: 1}
	funcs(events).Read = func(Conn) error { return nil }
	funcs(events).Close = func(_ Conn, err error) { closed <- err }
	const writes = 64
	const size = 4 << 10
	testConn, registered := startTestConnection(t, events)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	// The peer reads nothing yet, so the queue outruns the send buffer.
	if err := unix.SetsockoptInt(testConn.peer, unix.SOL_SOCKET, unix.SO_RCVBUF, 8<<10); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	var wrote atomic.Int64
	allQueued := make(chan struct{})
	go func() {
		defer close(allQueued)
		for range writes {
			if _, err := testConn.conn.Write(payload); err != nil {
				t.Errorf("Write: %v", err)
				return
			}
			wrote.Add(int64(size))
		}
	}()
	<-allQueued
	if err := testConn.conn.CloseGracefully(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := testConn.conn.Write(payload); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Write during drain: %v, want net.ErrClosed", err)
	}
	// The peer reads while the drain waits: that is what empties the queue.
	type result struct {
		got []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		got := make([]byte, 0, writes*size)
		buf := make([]byte, 32<<10)
		deadline := time.Now().Add(10 * time.Second)
		for len(got) < writes*size && time.Now().Before(deadline) {
			n, err := unix.Read(testConn.peer, buf)
			if n > 0 {
				got = append(got, buf[:n]...)
			}
			if err != nil && !errors.Is(err, unix.EAGAIN) {
				done <- result{got, err}
				return
			}
			if n == 0 {
				time.Sleep(time.Millisecond)
			}
		}
		done <- result{got, nil}
	}()
	var cause error
	select {
	case cause = <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("OnClose did not run once the queue drained")
	}
	if cause != nil {
		t.Fatalf("OnClose reported %v; want nil", cause)
	}
	res := <-done
	if res.err != nil {
		t.Fatalf("peer read: %v", res.err)
	}
	if len(res.got) != writes*size {
		t.Fatalf("peer read %d bytes, want %d", len(res.got), writes*size)
	}
	for i := range res.got {
		if res.got[i] != byte(i%size%251) {
			t.Fatalf("byte %d corrupted", i)
		}
	}
}

// A drain whose queue cannot leave — the peer reads nothing — ends at its
// bound and reports the leftover as UnflushedError.
func TestCloseGracefullyExpiresToUnflushed(t *testing.T) {
	saved := lingerTimeout
	lingerTimeout = 150 * time.Millisecond
	defer func() { lingerTimeout = saved }()

	closed := make(chan error, 1)
	events := &Events{Pollers: 1}
	funcs(events).Read = func(Conn) error { return nil }
	funcs(events).Close = func(_ Conn, err error) { closed <- err }
	testConn, registered := startTestConnection(t, events)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptInt(testConn.peer, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<10); err != nil {
		t.Fatal(err)
	}
	const total = 256 << 10
	payload := make([]byte, 64<<10)
	deadline := time.Now().Add(5 * time.Second)
	sent := 0
	for sent < total && time.Now().Before(deadline) {
		n, err := testConn.conn.Write(payload)
		sent += n
		if err != nil {
			break
		}
	}
	if sent == 0 {
		t.Fatal("nothing was accepted")
	}
	if err := testConn.conn.CloseGracefully(io.EOF); err != nil {
		t.Fatal(err)
	}
	var cause error
	select {
	case cause = <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("OnClose did not run at the drain's bound")
	}
	if !errors.Is(cause, ErrUnflushedData) {
		t.Fatalf("OnClose reported %v; want UnflushedError", cause)
	}
	if !errors.Is(cause, io.EOF) {
		t.Fatalf("OnClose reported %v; want the close's cause joined", cause)
	}
}

// A CloseWith during a drain ends the wait at once, as an abort does a
// graceful close's.
func TestCloseWithCutsADrainShort(t *testing.T) {
	saved := lingerTimeout
	lingerTimeout = 10 * time.Second
	defer func() { lingerTimeout = saved }()

	closed := make(chan error, 1)
	events := &Events{Pollers: 1}
	funcs(events).Read = func(Conn) error { return nil }
	funcs(events).Close = func(_ Conn, err error) { closed <- err }
	testConn, registered := startTestConnection(t, events)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptInt(testConn.peer, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<10); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 64<<10)
	for i := 0; i < 4; i++ {
		if _, err := testConn.conn.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := testConn.conn.CloseGracefully(nil); err != nil {
		t.Fatal(err)
	}
	if err := testConn.conn.CloseWith(io.EOF); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-closed:
		if !errors.Is(cause, io.EOF) {
			t.Fatalf("OnClose reported %v; want io.EOF", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the abort did not end the drain")
	}
}

// A drain whose queue is empty releases at once: nothing waits.
func TestCloseGracefullyWithEmptyQueueClosesAtOnce(t *testing.T) {
	saved := lingerTimeout
	lingerTimeout = 10 * time.Second // waiting out the bound must not look at-once
	defer func() { lingerTimeout = saved }()
	closed := make(chan error, 1)
	events := &Events{Pollers: 1}
	funcs(events).Read = func(Conn) error { return nil }
	funcs(events).Close = func(_ Conn, err error) { closed <- err }
	testConn, registered := startTestConnection(t, events)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	if err := testConn.conn.CloseGracefully(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-closed:
		if cause != nil {
			t.Fatalf("OnClose reported %v; want nil", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an empty drain did not release at once")
	}
}
