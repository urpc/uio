package uio

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitLifecycle(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Every ordering keeps a started OnOpen serialized with OnClose, preserves
// the first close cause, permits reentrant listener closes, and joins every
// callback before OnStop and Serve. OnStart pins the unregistered-listener
// case; it must have the same lifetime owner as an accepting listener.
func TestListenerCloseLifecycleOrderings(t *testing.T) {
	for _, order := range []string{"PLE", "PEL", "LPE", "LEP", "EPL", "ELP"} {
		t.Run(order, func(t *testing.T) {
			added := make(chan PacketListener, 1)
			leaveStart, leaveOpen := make(chan struct{}), make(chan struct{})
			opened, closed := make(chan struct{}), make(chan struct{})
			var releaseStart, releaseOpen sync.Once
			var inOpen, stopped atomic.Bool
			var opens, closes atomic.Int32
			var other Listener
			var cause error
			want := errors.New("peer requested close")
			ev := withFuncs(&Events{Pollers: 1}, FuncHandler{
				Start: func(ev *Events) {
					ln, err := ev.Listen("udp4://127.0.0.1:0", nil)
					if err != nil {
						t.Error(err)
						return
					}
					other, err = ev.Listen("tcp4://127.0.0.1:0", nil)
					if err != nil {
						t.Error(err)
						return
					}
					added <- ln.(PacketListener)
					<-leaveStart
				},
				Open: func(Conn) {
					opens.Add(1)
					inOpen.Store(true)
					close(opened)
					<-leaveOpen
					inOpen.Store(false)
				},
				Close: func(_ Conn, err error) {
					if inOpen.Load() || stopped.Load() {
						t.Error("OnClose overlapped OnOpen or ran after OnStop")
					}
					_ = other.Close()
					cause = err
					if closes.Add(1) == 1 {
						close(closed)
					}
				},
				Stop: func(*Events) { stopped.Store(true) },
			})
			serveDone := make(chan struct{})
			go func() { _ = ev.Serve(); close(serveDone) }()
			t.Cleanup(func() {
				releaseOpen.Do(func() { close(leaveOpen) })
				releaseStart.Do(func() { close(leaveStart) })
				_ = ev.Close(nil)
				waitLifecycle(t, serveDone, "Serve cleanup")
			})
			var ln PacketListener
			select {
			case ln = <-added:
			case <-time.After(5 * time.Second):
				t.Fatal("listener startup timed out")
			}
			peer, err := ln.DialPeer(netip.MustParseAddrPort("127.0.0.1:9876"), nil)
			if err != nil {
				t.Fatal(err)
			}
			waitLifecycle(t, opened, "OnOpen")
			for _, action := range order {
				switch action {
				case 'P':
					_ = peer.CloseWith(want)
				case 'L':
					_ = ln.Close()
				case 'E':
					_ = ev.Close(nil)
				}
			}
			releaseOpen.Do(func() { close(leaveOpen) })
			releaseStart.Do(func() { close(leaveStart) })
			waitLifecycle(t, serveDone, "Serve with reentrant OnClose")
			select {
			case <-closed:
			default:
				t.Fatal("Serve returned before OnClose")
			}
			if opens.Load() != 1 || closes.Load() != 1 {
				t.Fatalf("opens=%d closes=%d", opens.Load(), closes.Load())
			}
			if order[0] == 'P' && !errors.Is(cause, want) {
				t.Fatalf("peer's close cause = %v, want %v", cause, want)
			}
			if order[0] == 'L' && !errors.Is(cause, net.ErrClosed) {
				t.Fatalf("listener's close cause = %v, want net.ErrClosed", cause)
			}
		})
	}
}

// Socket creation calls application code. Closing Events or another listener
// from that code must not reenter a held lifecycle or listener-registry lock.
func TestListenControlMayClose(t *testing.T) {
	for _, network := range []string{"tcp4", "udp4"} {
		for _, target := range []string{"events", "listener"} {
			t.Run(network+"/"+target, func(t *testing.T) {
				started, serveDone := make(chan struct{}), make(chan struct{})
				ev := withFuncs(&Events{Pollers: 1}, FuncHandler{
					Start: func(*Events) { close(started) },
				})
				go func() { _ = ev.Serve(); close(serveDone) }()
				t.Cleanup(func() {
					go ev.Close(nil)
					waitLifecycle(t, serveDone, "Serve cleanup")
				})
				waitLifecycle(t, started, "OnStart")
				other, err := ev.Listen("udp4://127.0.0.1:0", nil)
				if err != nil {
					t.Fatal(err)
				}
				listenDone := make(chan struct{})
				var added Listener
				go func() {
					added, err = ev.Listen(network+"://127.0.0.1:0", nil,
						WithListenControl(func(string, string, uintptr) error {
							if target == "events" {
								return ev.Close(nil)
							}
							return other.Close()
						}))
					close(listenDone)
				}()
				waitLifecycle(t, listenDone, "Listen with reentrant close")
				if target == "events" {
					if added != nil || !errors.Is(err, net.ErrClosed) {
						t.Fatalf("Listen = %v, %v; want nil, net.ErrClosed", added, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					_ = added.Close()
					if _, err := other.(PacketListener).DialPeer(netip.MustParseAddrPort("127.0.0.1:9876"), nil); !errors.Is(err, net.ErrClosed) {
						t.Fatalf("DialPeer on closed listener = %v", err)
					}
				}
			})
		}
	}
}

// A Listen blocked in user socket setup owns only its unpublished sockets.
// Shutdown must finish without it, and it must reject admission afterward.
func TestListenControlShutdownAdmission(t *testing.T) {
	for _, network := range []string{"tcp4", "udp4"} {
		t.Run(network, func(t *testing.T) {
			started, serveDone := make(chan struct{}), make(chan struct{})
			ev := withFuncs(&Events{Pollers: 1}, FuncHandler{
				Start: func(*Events) { close(started) },
			})
			go func() { _ = ev.Serve(); close(serveDone) }()
			resume, entered := make(chan struct{}), make(chan struct{})
			var release sync.Once
			t.Cleanup(func() {
				release.Do(func() { close(resume) })
				go ev.Close(nil)
				waitLifecycle(t, serveDone, "Serve cleanup")
			})
			waitLifecycle(t, started, "OnStart")
			listenDone := make(chan struct{})
			go func() {
				ln, err := ev.Listen(network+"://127.0.0.1:0", nil,
					WithListenControl(func(string, string, uintptr) error {
						close(entered)
						<-resume
						return nil
					}))
				if ln != nil || !errors.Is(err, net.ErrClosed) {
					t.Errorf("Listen after shutdown = %v, %v; want nil, net.ErrClosed", ln, err)
				}
				close(listenDone)
			}()
			waitLifecycle(t, entered, "socket control")
			go ev.Close(nil)
			waitLifecycle(t, serveDone, "shutdown during socket control")
			release.Do(func() { close(resume) })
			waitLifecycle(t, listenDone, "rejected listener admission")
		})
	}
}

func TestConnControlMayClose(t *testing.T) {
	for _, network := range []string{"tcp4", "udp4"} {
		t.Run(network, func(t *testing.T) {
			started, serveDone := make(chan struct{}), make(chan struct{})
			opened, closed := make(chan struct{}), make(chan struct{})
			ev := withFuncs(&Events{Pollers: 1}, FuncHandler{
				Start: func(*Events) { close(started) },
				Open: func(c Conn) {
					if c.Userdata() == "control" {
						close(opened)
					}
				},
				Close: func(c Conn, _ error) {
					if c.Userdata() == "control" {
						close(closed)
					}
				},
			})
			go func() { _ = ev.Serve(); close(serveDone) }()
			t.Cleanup(func() {
				go ev.Close(nil)
				waitLifecycle(t, serveDone, "Serve cleanup")
			})
			waitLifecycle(t, started, "OnStart")
			ln, err := ev.Listen(network+"://127.0.0.1:0", nil)
			if err != nil {
				t.Fatal(err)
			}
			var conn Conn
			if network == "udp4" {
				conn, err = ln.(PacketListener).DialPeer(netip.MustParseAddrPort("127.0.0.1:9876"), "control")
			} else {
				conn, err = ev.Dial(network+"://"+ln.Addr().String(), "control")
			}
			if err != nil {
				t.Fatal(err)
			}
			waitLifecycle(t, opened, "OnOpen")
			controlled := make(chan struct{})
			go func() {
				if err := conn.Control(func(uintptr) {
					if network == "udp4" {
						err = ln.Close()
					} else {
						err = conn.Close()
					}
				}); err != nil {
					t.Error(err)
				}
				close(controlled)
			}()
			waitLifecycle(t, controlled, "Control with reentrant close")
			if err != nil {
				t.Fatal(err)
			}
			waitLifecycle(t, closed, "OnClose")
		})
	}
}
