package uws

import (
	"testing"
	"time"

	"github.com/urpc/uio/uws/internal/frame"
)

// A Pong accepted before a heartbeat Ping keeps its place ahead of it: only
// the Ping's own retirement starts the Pong timeout.
func TestHeartbeatPingPositionFollowsEarlierPong(t *testing.T) {
	raw := newScriptedConn()
	conn := &Conn{raw: raw, config: testServerConfig(NewServer(nil)), heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Ping, Payload: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	beforePing := conn.writes.close.pendingBytes.Load()
	if beforePing == 0 {
		t.Fatal("Pong was not queued")
	}
	if err := conn.sendHeartbeatPing(time.Now()); err != nil {
		t.Fatal(err)
	}
	pending := conn.writes.close.pendingBytes.Load()
	conn.heartbeat.mu.Lock()
	target, sentAt := conn.heartbeat.pingTarget, conn.heartbeat.pingSentAt
	conn.heartbeat.mu.Unlock()
	if pending <= beforePing || target != uint64(pending) || sentAt != 0 {
		t.Fatalf("queued Ping = pending:%d prior:%d target:%d sent:%d", pending, beforePing, target, sentAt)
	}
	conn.releaseOutbound(int(beforePing))
	conn.heartbeat.mu.Lock()
	sentAt = conn.heartbeat.pingSentAt
	conn.heartbeat.mu.Unlock()
	if sentAt != 0 {
		t.Fatal("Pong retirement marked Ping as sent")
	}
	conn.releaseOutbound(int(pending - beforePing))
	conn.heartbeat.mu.Lock()
	sentAt = conn.heartbeat.pingSentAt
	conn.heartbeat.mu.Unlock()
	if sentAt == 0 {
		t.Fatal("Ping retirement did not start Pong timeout")
	}
}
