package uws

import (
	"testing"
	"time"
)

// BenchmarkHeartbeatTickSteady is the per-turn cost a busy connection with a
// heartbeat pays when nothing is due: the shape every data turn sees.
func BenchmarkHeartbeatTickSteady(b *testing.B) {
	server := NewServer(Options{})
	server.HeartbeatInterval = time.Hour
	raw := newScriptedConn()
	conn := &Conn{raw: raw, config: testServerConfig(server), heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	conn.beginHeartbeat()
	now := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		conn.heartbeatTick(now)
	}
}
