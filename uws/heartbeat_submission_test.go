package uws

import (
	"errors"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

type controlledPongConn struct {
	*scriptedConn
	pongStarted chan struct{}
	releasePong chan struct{}
	rejectPong  bool
}

func (raw *controlledPongConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	if wire := buffer.Bytes(); len(wire) > 0 && wire[0]&0x0f == byte(frame.Pong) {
		close(raw.pongStarted)
		<-raw.releasePong
		if raw.rejectPong {
			uio.ReleaseBuffer(buffer)
			return 0, uio.ErrOutboundOverflow
		}
	}
	return raw.scriptedConn.WriteOwned(buffer)
}

func TestHeartbeatPingPositionAfterConcurrentPong(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "accepted"
		if reject {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			raw := &controlledPongConn{
				scriptedConn: newScriptedConn(),
				pongStarted:  make(chan struct{}),
				releasePong:  make(chan struct{}),
				rejectPong:   reject,
			}
			defer func() {
				select {
				case <-raw.releasePong:
				default:
					close(raw.releasePong)
				}
			}()
			conn := &Conn{raw: raw, config: testServerConfig(NewServer(nil)), heartbeat: &heartbeatState{}}
			conn.opened.Store(true)
			pongDone := make(chan error, 1)
			go func() {
				pongDone <- conn.sendConcurrentControlFrame(frame.Frame{Fin: true, Opcode: frame.Pong, Payload: []byte{1}})
			}()
			select {
			case <-raw.pongStarted:
			case <-time.After(testIOTimeout()):
				t.Fatal("Pong did not enter the blocked UIO submission")
			}

			type pingResult struct {
				attempted bool
				err       error
			}
			now := time.Now()
			pingDone := make(chan pingResult, 1)
			go func() {
				attempted, err := conn.tryHeartbeatPing(now)
				pingDone <- pingResult{attempted, err}
			}()
			select {
			case result := <-pingDone:
				if result.attempted || result.err != nil {
					t.Fatalf("Ping while Pong submits = %+v, want deferred attempt", result)
				}
			case <-time.After(testIOTimeout()):
				t.Fatal("heartbeat Ping waited for a concurrent control frame")
			}
			close(raw.releasePong)
			if err := <-pongDone; reject != errors.Is(err, ErrBackpressure) || (!reject && err != nil) {
				t.Fatalf("Pong result = %v, rejected=%v", err, reject)
			}
			beforePing := conn.writes.close.pendingBytes.Load()
			if reject && beforePing != 0 {
				t.Fatalf("rejected Pong left %d pending bytes", beforePing)
			}
			if attempted, err := conn.tryHeartbeatPing(now.Add(time.Millisecond)); !attempted || err != nil {
				t.Fatalf("Ping after Pong = attempted:%v err:%v", attempted, err)
			}
			pending := conn.writes.close.pendingBytes.Load()
			conn.heartbeat.mu.Lock()
			target, sentAt := conn.heartbeat.pingTarget, conn.heartbeat.pingSentAt
			conn.heartbeat.mu.Unlock()
			if pending <= beforePing || target != uint64(pending) || sentAt != 0 {
				t.Fatalf("queued Ping = pending:%d prior:%d target:%d sent:%d", pending, beforePing, target, sentAt)
			}
			if beforePing > 0 {
				conn.releaseOutbound(int(beforePing))
				conn.heartbeat.mu.Lock()
				sentAt = conn.heartbeat.pingSentAt
				conn.heartbeat.mu.Unlock()
				if sentAt != 0 {
					t.Fatal("Pong retirement marked Ping as sent")
				}
			}
			conn.releaseOutbound(int(pending - beforePing))
			conn.heartbeat.mu.Lock()
			sentAt = conn.heartbeat.pingSentAt
			conn.heartbeat.mu.Unlock()
			if sentAt == 0 {
				t.Fatal("Ping retirement did not start Pong timeout")
			}
		})
	}
}
