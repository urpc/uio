package kcp

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"slices"
	"testing"
	"time"
)

// Exercise public engine calls over an in-memory network with packet loss,
// duplication, jitter and reordering. The receive queues initially stop reading
// long enough to close their advertised windows and require window probing.
func TestEngineReliableBidirectionalDeliveryOverLossyNetwork(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, nodelay := range []int{0, 1} {
			name := "messages"
			if stream {
				name = "stream"
			}
			t.Run(name+string(rune('0'+nodelay)), func(t *testing.T) {
				clock := &fakeClock{ms: 1000}
				clockNow = func() uint32 { return clock.ms }
				t.Cleanup(func() { clockNow = nil })
				rng := rand.New(rand.NewSource(77))
				type packet struct {
					to   int
					at   uint32
					data []byte
				}
				var pending []packet
				var emitted int
				engines := [2]*KCP{}
				for side := range engines {
					engines[side] = NewKCP(17, func(p []byte, n int) {
						if n == 0 {
							return
						}
						emitted++
						if emitted%7 == 0 { // data and acknowledgments can both be lost
							return
						}
						item := packet{to: 1 - side, at: clock.ms + uint32(rng.Intn(45)), data: bytes.Clone(p[:n])}
						pending = append(pending, item)
						if emitted%11 == 0 {
							pending = append(pending, item)
						}
					})
					engines[side].SetMtu(140)
					engines[side].SetStreamMode(stream)
					engines[side].NoDelay(nodelay, 20, 2, 0)
					engines[side].WndSize(32, 16)
				}
				var want, got [2][]byte
				var messages [2][][]byte
				for side, engine := range engines {
					for i := 0; i < 60; i++ {
						p := make([]byte, 1+rng.Intn(850))
						rng.Read(p)
						messages[1-side] = append(messages[1-side], p)
						want[1-side] = append(want[1-side], p...)
						if engine.Send(p) != 0 {
							t.Fatal("valid application message was refused")
						}
					}
					if engine.QueuedBytes() != len(want[1-side]) || engine.Conv() != 17 {
						t.Fatal("queued byte accounting or conversation identity changed")
					}
				}
				indices := [2]int{}
				for step := 0; step < 30000; step++ {
					clock.advance(10 * time.Millisecond)
					for _, engine := range engines {
						if _itimediff(clock.ms, engine.Check()) >= 0 {
							engine.Update()
						}
					}
					flight := pending
					pending = nil
					for i := len(flight) - 1; i >= 0; i-- {
						item := flight[i]
						if _itimediff(clock.ms, item.at) < 0 {
							pending = append(pending, item)
							continue
						}
						if ret := engines[item.to].Input(item.data, IKCP_PACKET_REGULAR, step%2 == 0); ret != 0 {
							t.Fatalf("network emitted an invalid packet: %d", ret)
						}
					}
					if step < 800 { // the application holds the first receive windows
						continue
					}
					for side, engine := range engines {
						for engine.PeekSize() >= 0 {
							size := engine.PeekSize()
							p := make([]byte, size)
							if size > 0 && engine.Recv(p[:size-1]) != -2 {
								t.Fatal("short receive consumed a message")
							}
							if n := engine.Recv(p); n != size {
								t.Fatalf("Recv=%d, PeekSize=%d", n, size)
							}
							got[side] = append(got[side], p...)
							if !stream {
								if indices[side] >= len(messages[side]) || !bytes.Equal(p, messages[side][indices[side]]) {
									t.Fatal("message boundaries or ordering changed")
								}
								indices[side]++
							}
						}
					}
					if len(got[0]) == len(want[0]) && len(got[1]) == len(want[1]) && engines[0].WaitSnd() == 0 && engines[1].WaitSnd() == 0 {
						for side := range engines {
							if !bytes.Equal(got[side], want[side]) || engines[side].IsDead() || engines[side].CWnd() == 0 || engines[side].RemoteWnd() == 0 {
								t.Fatal("reliable delivery or congestion/window state did not recover")
							}
						}
						return
					}
				}
				t.Fatalf("network never recovered: received=%d/%d,%d/%d waiting=%d,%d", len(got[0]), len(want[0]), len(got[1]), len(want[1]), engines[0].WaitSnd(), engines[1].WaitSnd())
			})
		}
	}
}

func TestEngineInputValidationAndReceiveWindowReordering(t *testing.T) {
	engine := NewKCP(7, func([]byte, int) {})
	for _, tc := range []struct {
		packet []byte
		want   int
	}{
		{nil, -1}, {testSegment(8, IKCP_CMD_PUSH, 0, 2, 0, 0, nil), -1},
		{testSegment(7, 255, 0, 2, 0, 0, nil), -3},
	} {
		if got := engine.Input(tc.packet, IKCP_PACKET_REGULAR, false); got != tc.want {
			t.Fatalf("invalid input=%d, want %d", got, tc.want)
		}
	}
	truncated := testSegment(7, IKCP_CMD_PUSH, 0, 2, 0, 0, []byte("body"))
	if got := engine.Input(truncated[:len(truncated)-1], IKCP_PACKET_REGULAR, false); got != -2 {
		t.Fatalf("truncated payload input=%d", got)
	}
	engine.WndSize(4, 2)
	for _, sn := range []uint32{1, 1, 20, 0, 3} {
		if ret := engine.Input(testSegment(7, IKCP_CMD_PUSH, 0, 2, sn, 0, []byte{byte('a' + sn)}), IKCP_PACKET_REGULAR, false); ret != 0 {
			t.Fatal(ret)
		}
	}
	var got []byte
	buffer := make([]byte, 1)
	if n := engine.Recv(buffer); n != 1 || buffer[0] != 'a' {
		t.Fatal("out-of-order receive lost the first segment")
	}
	got = append(got, buffer[0])
	engine.Input(testSegment(7, IKCP_CMD_PUSH, 0, 2, 2, 0, []byte("c")), IKCP_PACKET_REGULAR, false)
	for engine.PeekSize() >= 0 {
		if engine.Recv(buffer) != 1 {
			t.Fatal("unexpected receive size")
		}
		got = append(got, buffer[0])
	}
	if string(got) != "abcd" || engine.Recv(buffer) != -1 {
		t.Fatalf("receive ordering/deduplication = %q", got)
	}
	engine.Input(testSegment(7, IKCP_CMD_ACK, 0, 99, 100, 0, nil), IKCP_PACKET_FEC, false)
	if engine.RemoteWnd() != 2 {
		t.Fatal("a recovered/FEC packet changed the peer's advertised window")
	}
}

func TestEngineConfigurationSchedulingAndWindowProbes(t *testing.T) {
	clock := &fakeClock{ms: 1000}
	clockNow = func() uint32 { return clock.ms }
	t.Cleanup(func() { clockNow = nil })
	var packets [][]byte
	engine := NewKCP(7, func(p []byte, n int) { packets = append(packets, bytes.Clone(p[:n])) })
	if engine.Send(nil) != -1 || engine.Send(make([]byte, 256*int(engine.mss))) != -2 || engine.SetMtu(IKCP_OVERHEAD) != -1 {
		t.Fatal("invalid application limits were accepted")
	}
	engine.SetStreamMode(true)
	engine.NoDelay(1, 50000, 2, 1)
	if engine.interval != 5000 {
		t.Fatal("maximum update interval was not bounded")
	}
	engine.NoDelay(-1, -1, -1, -1)
	engine.SetLogger(IKCP_LOG_ALL, func(string, ...any) {})
	engine.SetLogger(IKCP_LOG_ALL, nil)
	if engine.logmask != 0 || engine.Check() != clock.ms {
		t.Fatal("logger disable or initial scheduling failed")
	}
	engine.Send([]byte("a"))
	engine.Send([]byte("b"))
	if engine.Queued() != 1 || engine.QueuedBytes() != 2 {
		t.Fatal("stream writes were not coalesced")
	}
	engine.Update()
	clock.advance(100 * time.Millisecond)
	if got := engine.Check(); got <= clock.ms || got > clock.ms+200 {
		t.Fatalf("Check missed the nearest retransmission: now=%d next=%d", clock.ms, got)
	}
	clock.advance(150 * time.Millisecond)
	if engine.Check() != clock.ms {
		t.Fatal("an expired retransmission was not immediately runnable")
	}
	clock.advance(15 * time.Second)
	if engine.Check() != clock.ms {
		t.Fatal("large forward clock jump did not request an update")
	}
	engine.Update()
	clock.ms = 100
	engine.Update()
	engine.NoDelay(0, 1, 0, 0)
	if engine.interval != 10 {
		t.Fatal("minimum update interval was not bounded")
	}
	engine.Input(testSegment(7, IKCP_CMD_WINS, 0, 0, 0, 0, nil), IKCP_PACKET_REGULAR, false)
	engine.Flush()
	packets = nil
	for range 12 {
		clock.advance(120 * time.Second)
		engine.Flush()
	}
	found := false
	for _, p := range packets {
		for len(p) >= IKCP_OVERHEAD {
			found = found || p[4] == IKCP_CMD_WASK
			p = p[IKCP_OVERHEAD+binary.LittleEndian.Uint32(p[20:]):]
		}
	}
	if !found || engine.probe_wait > IKCP_PROBE_LIMIT {
		t.Fatal("zero-window probing did not back off within its limit")
	}
	engine.AskWindow()
	engine.FlushAckOnly()
	engine.Input(testSegment(7, IKCP_CMD_WASK, 0, 3, 0, 0, nil), IKCP_PACKET_REGULAR, false)
	engine.FlushAckOnly()
	if engine.probe_wait != 0 || engine.RemoteWnd() != 3 {
		t.Fatal("reopening the window did not reset probing")
	}
}

func TestEngineMTUChangeRetainsQueuedAndInflightSegments(t *testing.T) {
	var wire [][]byte
	engine := NewKCP(7, func(p []byte, n int) {
		if n > 0 {
			wire = append(wire, bytes.Clone(p[:n]))
		}
	})
	engine.NoDelay(1, 10, 0, 1)
	engine.Send(bytes.Repeat([]byte("a"), 1000))
	engine.Flush()
	engine.Send(bytes.Repeat([]byte("b"), 1000))
	if engine.SetMtu(60) != 0 || len(engine.buffer) < IKCP_OVERHEAD+1000 {
		t.Fatal("MTU change invalidated a retained large segment")
	}
	engine.Flush()
	peer := NewKCP(7, func([]byte, int) {})
	for _, p := range wire {
		if ret := peer.Input(p, IKCP_PACKET_REGULAR, false); ret != 0 {
			t.Fatal(ret)
		}
	}
	var got []byte
	for peer.PeekSize() >= 0 {
		p := make([]byte, peer.PeekSize())
		peer.Recv(p)
		got = append(got, p...)
	}
	want := append(bytes.Repeat([]byte("a"), 1000), bytes.Repeat([]byte("b"), 1000)...)
	if !slices.Equal(got, want) {
		t.Fatal("changing MTU corrupted already accepted application messages")
	}
}
