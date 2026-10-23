//go:build windows || stdio

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

func TestStdRejectedCloseReturnsInsideControl(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	remote, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	closed := make(chan struct{})
	ev := withFuncs(&Events{}, FuncHandler{Close: func(Conn, error) { close(closed) }})
	if err := ev.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	loop.beginStop(nil) // The request must use the fallback, with no loop running.
	fc := &fdConn{commonConn: commonConn{events: ev, loop: loop}, conn: conn, opened: true}
	fc.retainLifecycle()
	done := make(chan struct{})
	go func() {
		if err := fc.Control(func(uintptr) {
			if err := fc.Close(); err != nil {
				t.Error(err)
			}
		}); err != nil {
			t.Error(err)
		}
		close(done)
	}()
	waitLifecycle(t, done, "Close from Control")
	waitLifecycle(t, closed, "OnClose")
	ev.callbackWG.Wait()
}

// Pin both sides of a close's ownership transfer: before queue submission,
// after replacement removed its address entry, and after resource release,
// before callback scheduling. Neither gap may let shutdown finish early.
func TestStdCloseHandoffsJoinLifecycle(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "released"
		if replace {
			name = "replaced-before-submit"
		}
		t.Run(name, func(t *testing.T) {
			added := make(chan PacketListener, 1)
			leaveStart := make(chan struct{})
			opened, closed := make(chan struct{}), make(chan struct{})
			var stopped atomic.Bool
			var cause error
			ev := withFuncs(&Events{Pollers: 1}, FuncHandler{
				Start: func(ev *Events) {
					ln, err := ev.Listen("udp4://127.0.0.1:0", nil)
					if err != nil {
						t.Error(err)
						return
					}
					added <- ln.(PacketListener)
					<-leaveStart
				},
				Open: func(c Conn) {
					if c.Userdata() == "old" {
						close(opened)
					}
				},
				Close: func(c Conn, err error) {
					if c.Userdata() == "old" {
						if stopped.Load() {
							t.Error("OnClose ran after OnStop")
						}
						cause = err
						close(closed)
					}
				},
				Stop: func(*Events) { stopped.Store(true) },
			})
			serveDone := make(chan struct{})
			go func() { _ = ev.Serve(); close(serveDone) }()
			resume := make(chan struct{})
			var releaseStart, releaseClose sync.Once
			t.Cleanup(func() {
				releaseClose.Do(func() { close(resume) })
				releaseStart.Do(func() { close(leaveStart) })
				_ = ev.Close(nil)
				waitLifecycle(t, serveDone, "Serve cleanup")
				testHookStdCloseRequested.Store(nil)
				testHookStdCloseReleased.Store(nil)
			})
			ln := <-added
			addr := netip.MustParseAddrPort("127.0.0.1:9876")
			peer, err := ln.DialPeer(addr, "old")
			if err != nil {
				t.Fatal(err)
			}
			waitLifecycle(t, opened, "OnOpen")
			paused := make(chan struct{})
			hook := func(c *fdConn) {
				if c == peer {
					close(paused)
					<-resume
				}
			}
			if replace {
				testHookStdCloseRequested.Store(&hook)
			} else {
				testHookStdCloseReleased.Store(&hook)
				_ = ev.Close(nil) // Seal the queue so the caller owns the fallback.
			}
			want := errors.New("original close cause")
			closeDone := make(chan struct{})
			go func() {
				if err := peer.CloseWith(want); err != nil {
					t.Error(err)
				}
				close(closeDone)
			}()
			waitLifecycle(t, paused, "close hand-off")
			if replace {
				if _, err := ln.DialPeer(addr, "replacement"); err != nil {
					t.Fatal(err)
				}
				_ = ev.Close(nil)
			}
			releaseStart.Do(func() { close(leaveStart) })
			select {
			case <-serveDone:
				t.Error("Serve returned with the terminal close still paused")
			case <-time.After(30 * time.Millisecond):
			}
			releaseClose.Do(func() { close(resume) })
			waitLifecycle(t, closeDone, "CloseWith")
			waitLifecycle(t, serveDone, "Serve")
			select {
			case <-closed:
			default:
				t.Fatal("OnClose was lost")
			}
			if !errors.Is(cause, want) {
				t.Fatalf("close cause = %v, want %v", cause, want)
			}
		})
	}
}
