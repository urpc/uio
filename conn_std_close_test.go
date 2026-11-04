//go:build windows || stdio

package uio

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
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
	fc := &fdConn{commonConn: commonConn{loop: loop}, conn: conn, opened: true}
	fc.entry = ev.plainEntry()
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
					ln, err := ev.Listen("udp4://127.0.0.1:0")
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

// A close requested from outside is counted before it is published: a
// shutdown that sees it leaves the close to the request, and Serve's join
// waits for that.
func TestCloseRequestCountedBeforeShutdownSeesIt(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	ev := dialLayered(t, addr, nil, nil)
	conn, err := ev.DialContext(context.Background(), "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	socket := conn.(*fdConn)
	requested := make(chan struct{})
	resume := make(chan struct{})
	hook := func(c *fdConn) {
		if c == socket {
			close(requested)
			<-resume
		}
	}
	testHookStdCloseRequested.Store(&hook)
	defer testHookStdCloseRequested.Store(nil)
	closed := make(chan error, 1)
	go func() { closed <- conn.CloseWith(errors.New("canceled")) }()
	<-requested
	_ = ev.Close(nil)
	joined := make(chan struct{})
	go func() { _ = ev.Wait(); close(joined) }()
	select {
	case <-joined:
		close(resume)
		t.Fatal("Wait returned while a requested close was still being handed on")
	case <-time.After(50 * time.Millisecond):
	}
	close(resume)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	<-joined
}

// Writes refused while a CloseWrite waits schedule no turns: with the turns
// held up by a slow inbound Observer, each would otherwise leave a goroutine
// waiting for the callback lock.
func TestRefusedWritesScheduleNoTurns(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	say := make(chan struct{})
	go func() {
		if peer, err := listener.Accept(); err == nil {
			defer peer.Close()
			<-say
			_, _ = peer.Write([]byte("!"))
			_, _ = io.Copy(io.Discard, peer)
		}
	}()
	busy, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	addr := listener.Addr().String()
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return &windowFlush{} })}, func(ev *Events) {

		ev.Observer = FuncHandler{BytesRead: func(Conn, int) {
			once.Do(func() { close(busy) })
			<-release
		}}
	})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("held")); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil { // waits: the window holds "held"
		t.Fatal(err)
	}
	close(say)
	layerWaitFor(t, busy, "the busy Observer")
	before := runtime.NumGoroutine()
	for range 1000 {
		if _, err := conn.Write([]byte("no")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("a write after CloseWrite = %v, want net.ErrClosed", err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if added := runtime.NumGoroutine() - before; added > 10 {
		t.Fatalf("1000 refused writes left %d goroutines waiting", added)
	}
}
