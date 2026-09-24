package uws

import (
	"errors"
	"testing"

	"github.com/urpc/uio/uws/internal/frame"
)

type scriptedYieldConn struct{ *scriptedConn }

func (raw *scriptedYieldConn) YieldRead() error { return raw.Wake() }

func TestOnDataPropagatesUnrelatedErrorWhileClosing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client bool
	}{
		{name: "server"},
		{name: "client", client: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wakeErr := errors.New("yield failed")
			raw := &scriptedYieldConn{newScriptedConn()}
			raw.wakeErr = wakeErr
			conn := &Conn{raw: raw}
			if tc.client {
				conn.config = testDialerConfig(NewDialer())
			} else {
				conn.config = testServerConfig(NewServer(nil))
			}
			conn.opened.Store(true)
			seen := 0
			conn.handler = handlerFuncs{onMessage: func(_ *Conn, _ Message) {
				seen++
				if seen == maxFramesPerDataEvent {
					conn.closing.Store(true)
				}
			}}
			for i := 0; i < maxFramesPerDataEvent+1; i++ {
				raw.inbound = frame.Append(raw.inbound, frame.Frame{
					Fin: true, Opcode: frame.Binary, Masked: !tc.client, Payload: []byte{byte(i)},
				}, [4]byte{1, 2, 3, 4})
			}
			raw.userdata = conn
			var err error
			if tc.client {
				err = (&Dialer{}).onData(raw)
			} else {
				err = (&Server{}).onData(raw)
			}
			if !errors.Is(err, wakeErr) {
				t.Fatalf("OnData error = %v, want %v", err, wakeErr)
			}
			if seen != maxFramesPerDataEvent {
				t.Fatalf("messages = %d, want %d", seen, maxFramesPerDataEvent)
			}
		})
	}
}

func TestOnDataPropagatesFailedProtocolClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client bool
	}{
		{name: "server"},
		{name: "client", client: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeErr := errors.New("close frame write failed")
			raw := newScriptedConn()
			raw.writeErr = writeErr
			raw.inbound = frame.Append(nil, frame.Frame{
				Fin: true, Opcode: frame.Binary, Masked: tc.client,
			}, [4]byte{1, 2, 3, 4})
			conn := testServerConn(raw)
			if tc.client {
				conn.config = testDialerConfig(NewDialer())
			}
			raw.userdata = conn
			var err error
			if tc.client {
				err = (&Dialer{}).onData(raw)
			} else {
				err = (&Server{}).onData(raw)
			}
			if !errors.Is(err, frame.ErrProtocol) || !errors.Is(err, writeErr) {
				t.Fatalf("OnData error = %v, want protocol and write errors", err)
			}
			if raw.closes != 1 {
				t.Fatalf("transport closes = %d, want 1", raw.closes)
			}
		})
	}
}

func TestOnDataLeavesOwnedProtocolCloseToTransport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client bool
	}{
		{name: "server"},
		{name: "client", client: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := newScriptedConn()
			raw.inbound = frame.Append(nil, frame.Frame{
				Fin: true, Opcode: frame.Binary, Masked: tc.client,
			}, [4]byte{1, 2, 3, 4})
			conn := testServerConn(raw)
			if tc.client {
				conn.config = testDialerConfig(NewDialer())
			}
			raw.userdata = conn
			var err error
			if tc.client {
				err = (&Dialer{}).onData(raw)
			} else {
				err = (&Server{}).onData(raw)
			}
			if err != nil {
				t.Fatalf("OnData error = %v, want graceful close ownership", err)
			}
			if !conn.closing.Load() || len(raw.written) != 1 || raw.closes != 0 {
				t.Fatalf("close state = closing:%v writes:%d closes:%d", conn.closing.Load(), len(raw.written), raw.closes)
			}
			if info := conn.closeInfo(); !errors.Is(info.Err, frame.ErrProtocol) {
				t.Fatalf("recorded protocol error = %v, want %v", info.Err, frame.ErrProtocol)
			}
			completeTestOutbound(conn)
			if raw.closes != 1 {
				t.Fatalf("transport closes after outbound drain = %d, want 1", raw.closes)
			}
		})
	}
}
