//go:build windows || stdio

package uio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

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
			ev := withFuncs(&Events{Pollers: 1}, FuncHandler{Start: func(ev *Events) {
				ln, err := ev.Listen("udp4://127.0.0.1:0")
				if err != nil {
					t.Error(err)
					return
				}
				added <- ln.(PacketListener)
				<-leaveStart
			}, Open: func(c Conn) {
				if c.Userdata() == "old" {
					close(opened)
				}
			}, Close: func(c Conn, err error) {
				if c.Userdata() == "old" {
					if stopped.Load() {
						t.Error("OnClose ran after OnStop")
					}
					cause = err
					close(closed)
				}
			}, Stop: func(*Events) {
				stopped.Store(true)
			}})
			serveDone := make(chan struct{})
			go func() {
				_ = ev.Serve()
				close(serveDone)
			}()
			resume := make(chan struct{})
			var releaseStart, releaseClose sync.Once
			t.Cleanup(func() {
				releaseClose.Do(func() {
					close(resume)
				})
				releaseStart.Do(func() {
					close(leaveStart)
				})
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
				_ = ev.Close(nil)
			}
			want := errors.New("original close cause")
			closeDone := make(chan struct{})
			go func() {
				if err := peer.Abort(want); err != nil {
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
			releaseStart.Do(func() {
				close(leaveStart)
			})
			select {
			case <-serveDone:
				t.Error("Serve returned with the terminal close still paused")
			case <-time.After(30 * time.Millisecond):
			}
			releaseClose.Do(func() {
				close(resume)
			})
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
	go func() {
		closed <- conn.Abort(errors.New("canceled"))
	}()
	<-requested
	_ = ev.Close(nil)
	joined := make(chan struct{})
	go func() {
		_ = ev.Wait()
		close(joined)
	}()
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
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return &sendWindow{}
	})}, func(ev *Events) {
		ev.Observer = FuncHandler{BytesRead: func(Conn, int) {
			once.Do(func() {
				close(busy)
			})
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
	if err := conn.CloseWrite(); err != nil {
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

type stdRegistrationConn struct {
	fd             uintptr
	closed         chan struct{}
	readData       chan []byte
	closeOnce      sync.Once
	openReturned   atomic.Bool
	orderViolation atomic.Bool
	readActive     atomic.Int32
	maxReadActive  atomic.Int32
	readCalls      atomic.Int32
	secondRead     chan struct{}
	secondReadOnce sync.Once
	writeCalls     atomic.Int32
	writeStarted   chan struct{}
	writeRelease   chan struct{}
	writeOnce      sync.Once
	writeFunc      func([]byte) (int, error)
	readFunc       func([]byte) (int, error)
}

func newStdRegistrationConn(fd uintptr) *stdRegistrationConn {
	return &stdRegistrationConn{fd: fd, closed: make(chan struct{}), readData: make(chan []byte)}
}

func (conn *stdRegistrationConn) Read(buffer []byte) (int, error) {
	if conn.readCalls.Add(1) == 2 && conn.secondRead != nil {
		conn.secondReadOnce.Do(func() {
			close(conn.secondRead)
		})
	}
	active := conn.readActive.Add(1)
	defer conn.readActive.Add(-1)
	for maximum := conn.maxReadActive.Load(); active > maximum; maximum = conn.maxReadActive.Load() {
		if conn.maxReadActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	if !conn.openReturned.Load() {
		conn.orderViolation.Store(true)
	}
	if conn.readFunc != nil {
		return conn.readFunc(buffer)
	}
	select {
	case data := <-conn.readData:
		return copy(buffer, data), nil
	case <-conn.closed:
		return 0, net.ErrClosed
	}
}

func (conn *stdRegistrationConn) Write(buffer []byte) (int, error) {
	conn.writeCalls.Add(1)
	if !conn.openReturned.Load() {
		conn.orderViolation.Store(true)
	}
	if conn.writeStarted != nil {
		conn.writeOnce.Do(func() {
			close(conn.writeStarted)
		})
		<-conn.closed
		if conn.writeRelease != nil {
			<-conn.writeRelease
		}
		return 0, net.ErrClosed
	}
	if conn.writeFunc != nil {
		return conn.writeFunc(buffer)
	}
	return len(buffer), nil
}

func (conn *stdRegistrationConn) Close() error {
	conn.closeOnce.Do(func() {
		close(conn.closed)
	})
	return nil
}

func (*stdRegistrationConn) LocalAddr() net.Addr {
	return &net.TCPAddr{}
}

func (*stdRegistrationConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{}
}

func (*stdRegistrationConn) SetDeadline(time.Time) error {
	return nil
}

func (*stdRegistrationConn) SetReadDeadline(time.Time) error {
	return nil
}

func (*stdRegistrationConn) SetWriteDeadline(time.Time) error {
	return nil
}

func (conn *stdRegistrationConn) SyscallConn() (syscall.RawConn, error) {
	return stdRawConn(conn.fd), nil
}

type stdRawConn uintptr

func (fd stdRawConn) Control(callback func(uintptr)) error {
	callback(uintptr(fd))
	return nil
}

func (fd stdRawConn) Read(callback func(uintptr) bool) error {
	callback(uintptr(fd))
	return nil
}

func (fd stdRawConn) Write(callback func(uintptr) bool) error {
	callback(uintptr(fd))
	return nil
}

func startStdTestEvents(t *testing.T, events *Events) <-chan error {
	t.Helper()
	started := make(chan struct{})
	funcs(events).Start = func(*Events) {
		close(started)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- events.Serve()
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stdio events did not start")
	}
	return serveDone
}

func newStdFDConn(events *Events, raw *stdRegistrationConn) *fdConn {
	conn := &fdConn{conn: raw, writeSig: make(chan struct{}, 1)}
	conn.entry = events.plainEntry()
	conn.loop = events.workers[0]
	return conn
}

func stopStdTestEvents(t *testing.T, events *Events, serveDone <-chan error, closeErr error) {
	t.Helper()
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- events.Close(closeErr)
	}()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for closeDone != nil || serveDone != nil {
		select {
		case err := <-closeDone:
			if err != nil {
				t.Fatalf("Events.Close error = %v", err)
			}
			closeDone = nil
		case err := <-serveDone:
			if !errors.Is(err, closeErr) {
				t.Fatalf("Events.Serve error = %v", err)
			}
			serveDone = nil
		case <-timer.C:
			t.Fatal("stdio events did not stop within one second")
		}
	}
}

func TestReadControlResumeBeforeOnOpenStartsSocketReads(t *testing.T) {
	for _, layered := range []bool{false, true} {
		name := "plain"
		if layered {
			name = "layered"
		}
		t.Run(name, func(t *testing.T) {
			raw := newStdRegistrationConn(30109)
			replayed := make(chan struct{}, 1)
			data := make(chan string, 1)
			events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(Conn) {
				raw.openReturned.Store(true)
			}, Read: func(c Conn) error {
				if c.InboundBuffered() == 0 {
					select {
					case replayed <- struct{}{}:
					default:
					}
					return nil
				}
				buffer := make([]byte, c.InboundBuffered())
				_, err := c.Read(buffer)
				data <- string(buffer)
				return err
			}}}
			done := startStdTestEvents(t, events)
			t.Cleanup(func() {
				_ = raw.Close()
				stopStdTestEvents(t, events, done, nil)
			})
			conn := newStdFDConn(events, raw)
			conn.readResume = make(chan struct{}, 1)
			if layered {
				entry, err := events.entryFor([]Layer{PassThroughLayer}, true)
				if err != nil {
					t.Fatal(err)
				}
				conn.entry = entry
			}
			if err := conn.PauseRead(); err != nil {
				t.Fatal(err)
			}
			if err := conn.ResumeRead(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 5*time.Second, func() bool {
				conn.callbackMu.Lock()
				defer conn.callbackMu.Unlock()
				return conn.wakesAfterOpen != 0
			})
			if err := events.addConn(conn); err != nil {
				t.Fatal(err)
			}
			readControlAwait(t, replayed, "the pre-open resume replay")
			select {
			case raw.readData <- []byte("first-input"):
			case <-time.After(5 * time.Second):
				t.Fatal("pre-open resume left the first socket read blocked")
			}
			if got := readControlAwait(t, data, "input after the replayed resume"); got != "first-input" {
				t.Fatalf("received %q, want first-input", got)
			}
		})
	}
}

func queuedStdConnection(t *testing.T, limit int) *fdConn {
	t.Helper()
	events := &Events{MaxOutboundBuffered: limit, readBufSize: 4096}
	conn := &fdConn{commonConn: commonConn{entry: events.plainEntry()}, conn: newStdRegistrationConn(0), writeSig: make(chan struct{}, 1)}
	t.Cleanup(func() {
		conn.outbound.Reset()
	})
	return conn
}

func TestStdSocketOptionsPreserveTCPAndLifecycleErrors(t *testing.T) {
	first, second := tcpConnectionPair(t)
	defer first.Close()
	defer second.Close()
	live := &fdConn{conn: first}
	unsupported := &fdConn{conn: newStdRegistrationConn(0)}
	closed := &fdConn{conn: first}
	closed.closing.Store(true)
	options := []struct {
		name string
		set  func(*fdConn) error
	}{{"linger", func(c *fdConn) error {
		return c.SetLinger(-1)
	}}, {"no-delay", func(c *fdConn) error {
		return c.SetNoDelay(true)
	}}, {"keep-alive", func(c *fdConn) error {
		return c.SetKeepAlive(true)
	}}, {"keep-alive-period", func(c *fdConn) error {
		return c.SetKeepAlivePeriod(7)
	}}, {"read-buffer", func(c *fdConn) error {
		return c.SetReadBuffer(8192)
	}}, {"write-buffer", func(c *fdConn) error {
		return c.SetWriteBuffer(8192)
	}}}
	for _, option := range options {
		t.Run(option.name, func(t *testing.T) {
			if err := option.set(live); err != nil {
				t.Fatalf("live TCP option = %v", err)
			}
			if err := option.set(unsupported); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("custom transport option = %v", err)
			}
			if err := option.set(closed); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("closed lifecycle option = %v", err)
			}
		})
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for _, option := range options {
		if err := option.set(live); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("%s underlying TCP close = %v", option.name, err)
		}
	}
}

func TestStdVectorAdmissionCopiesOnlyAcceptedInput(t *testing.T) {
	for _, segments := range []int{2, 10} {
		t.Run(fmt.Sprint("segments=", segments), func(t *testing.T) {
			conn := queuedStdConnection(t, 0)
			vec := make([][]byte, segments)
			var want []byte
			for i := range vec {
				vec[i] = bytes.Repeat([]byte{byte('a' + i)}, 4096)
				want = append(want, vec[i]...)
			}
			vec = append(vec, nil)
			n, err := conn.Writev(vec)
			if n != len(want) || err != nil {
				t.Fatalf("Writev = %d, %v", n, err)
			}
			for _, part := range vec {
				clear(part)
			}
			var got bytes.Buffer
			if _, err := conn.outbound.WriteTo(&got); err != nil || !bytes.Equal(got.Bytes(), want) {
				t.Fatal("queued vector retained or reordered caller storage")
			}
		})
	}
	conn := queuedStdConnection(t, 4)
	if n, err := conn.Writev([][]byte{nil, {}}); n != 0 || err != nil || conn.OutboundBuffered() != 0 {
		t.Fatal("empty vector changed queue state")
	}
	if n, err := conn.Writev([][]byte{[]byte("ab"), nil, []byte("c")}); n != 3 || err != nil {
		t.Fatalf("small vector = %d, %v", n, err)
	}
	if err := conn.WriteByte('d'); err != nil {
		t.Fatal(err)
	}
	if n, err := conn.Writev([][]byte{[]byte("e")}); n != 0 || !errors.Is(err, ErrOutboundOverflow) || conn.OutboundBuffered() != 4 {
		t.Fatal("small overflow was not refused whole")
	}
	var got bytes.Buffer
	_, _ = conn.outbound.WriteTo(&got)
	if got.String() != "abcd" {
		t.Fatalf("accepted output = %q", got.String())
	}
}

func TestStdVectorAndOwnedWritesRespectClosingAndExistingBudget(t *testing.T) {
	for _, large := range []bool{false, true} {
		size := 8
		if large {
			size = 8192
		}
		for _, state := range []string{"closing", "draining", "write-shut", "overflow"} {
			t.Run(fmt.Sprintf("large=%t/%s", large, state), func(t *testing.T) {
				conn := queuedStdConnection(t, size)
				if _, err := conn.Write([]byte("old")); err != nil {
					t.Fatal(err)
				}
				switch state {
				case "closing":
					conn.closing.Store(true)
				case "draining":
					conn.draining.Store(true)
				case "write-shut":
					conn.writeShut = true
				}
				want := net.ErrClosed
				if state == "overflow" {
					want = ErrOutboundOverflow
				}
				vec := [][]byte{bytes.Repeat([]byte{'v'}, size/2), bytes.Repeat([]byte{'w'}, size-size/2)}
				if n, err := conn.Writev(vec); n != 0 || !errors.Is(err, want) {
					t.Fatalf("Writev = %d, %v, want %v", n, err, want)
				}
				owned := AcquireBuffer(size)
				_, _ = owned.Write(bytes.Repeat([]byte{'o'}, size))
				if n, err := conn.WriteOwned(owned); n != 0 || !errors.Is(err, want) {
					t.Fatalf("WriteOwned = %d, %v, want %v", n, err, want)
				}
				if state == "overflow" {
					if owned.Len() != size {
						t.Fatal("overflow consumed caller ownership")
					}
					ReleaseBuffer(owned)
				}
				if conn.OutboundBuffered() != 3 {
					t.Fatal("rejected output changed the accepted prefix")
				}
			})
		}
	}
	conn := queuedStdConnection(t, 1)
	if n, err := conn.Writev([][]byte{make([]byte, 4096)}); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatal("oversized vector passed the write budget")
	}
	if n, err := conn.WriteOwned(nil); n != 0 || err != nil {
		t.Fatal("nil ownership was not a no-op")
	}
	if n, err := conn.WriteOwned(AcquireBuffer(0)); n != 0 || err != nil {
		t.Fatal("empty ownership was not consumed as a no-op")
	}
}

func TestStdConnectedDatagramsPreservePacketsAndReadFailure(t *testing.T) {
	receiver, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	payloads := make(chan string, 4)
	closed := make(chan error, 1)
	cause := errors.New("packet consumer stopped")
	events := &Events{Pollers: 1, Handler: FuncHandler{Read: func(c Conn) error {
		payload := string(c.PeekChunk())
		payloads <- payload
		if payload == "stop" {
			return cause
		}
		return nil
	}, Close: func(_ Conn, err error) {
		closed <- err
	}}}
	done := startStdTestEvents(t, events)
	t.Cleanup(func() {
		stopStdTestEvents(t, events, done, nil)
	})
	conn, err := events.Dial("udp://" + receiver.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := conn.Writev([][]byte{nil, []byte("one"), {}, []byte("two")}); n != 6 || err != nil {
		t.Fatalf("packet vector = %d, %v", n, err)
	}
	var sender *net.UDPAddr
	readPacket := func(want string) {
		t.Helper()
		_ = receiver.SetReadDeadline(time.Now().Add(time.Second))
		buffer := make([]byte, 128)
		n, remote, err := receiver.ReadFromUDP(buffer)
		if err != nil || string(buffer[:n]) != want {
			t.Fatalf("packet = %q, %v, want %q", buffer[:n], err, want)
		}
		sender = remote
	}
	readPacket("one")
	readPacket("two")
	owned := AcquireBuffer(5)
	_, _ = owned.WriteString("owned")
	if n, err := conn.WriteOwned(owned); n != 5 || err != nil {
		t.Fatalf("owned packet = %d, %v", n, err)
	}
	readPacket("owned")
	if n, err := conn.Writev([][]byte{[]byte("ok"), make([]byte, 70<<10)}); n != 2 || err == nil {
		t.Fatalf("partial packet vector = %d, %v", n, err)
	}
	readPacket("ok")
	if conn.OutboundBuffered() != 0 {
		t.Fatal("datagram writes created a stream queue")
	}
	for _, payload := range []string{"first", "second", "stop"} {
		if _, err := receiver.WriteToUDP([]byte(payload), sender); err != nil {
			t.Fatal(err)
		}
		if got := readControlAwait(t, payloads, "connected packet input"); got != payload {
			t.Fatalf("unconsumed earlier packet leaked into %q", got)
		}
	}
	if err := readControlAwait(t, closed, "packet callback failure"); !errors.Is(err, cause) {
		t.Fatalf("packet OnClose = %v", err)
	}
}

type stdDeadlineFailureConn struct {
	*stdRegistrationConn
	cause error
	calls []string
}

func (c *stdDeadlineFailureConn) SetDeadline(time.Time) error {
	c.calls = append(c.calls, "both")
	return c.cause
}

func (c *stdDeadlineFailureConn) SetReadDeadline(time.Time) error {
	c.calls = append(c.calls, "read")
	return c.cause
}

func (c *stdDeadlineFailureConn) SetWriteDeadline(time.Time) error {
	c.calls = append(c.calls, "write")
	return c.cause
}

type stdControlFailureConn struct {
	*stdRegistrationConn
	cause error
}

func (c *stdControlFailureConn) SyscallConn() (syscall.RawConn, error) {
	return nil, c.cause
}

func TestStdFailedDeadlineDoesNotPublishASuccessfulTimer(t *testing.T) {
	cause := errors.New("transport rejected deadline")
	raw := &stdDeadlineFailureConn{stdRegistrationConn: newStdRegistrationConn(0), cause: cause}
	conn := &fdConn{conn: raw, readResume: make(chan struct{}, 1)}
	when := time.Now().Add(time.Hour)
	for _, set := range []func(time.Time) error{conn.SetDeadline, conn.SetReadDeadline, conn.SetWriteDeadline} {
		if err := set(when); !errors.Is(err, cause) {
			t.Fatalf("deadline = %v", err)
		}
	}
	if len(raw.calls) != 3 || raw.calls[0] != "both" || raw.calls[1] != "read" || raw.calls[2] != "write" {
		t.Fatalf("transport deadline calls = %v", raw.calls)
	}
	if !conn.readDeadline.IsZero() || !conn.writeDeadline.IsZero() || conn.writeTimer != nil || len(conn.readResume) != 0 {
		t.Fatal("a rejected deadline armed a timer or changed a held-read deadline")
	}
	if err := (&fdConn{}).SetDeadline(when); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing transport deadline = %v", err)
	}
	conn.closing.Store(true)
	if err := conn.SetReadDeadline(when); !errors.Is(err, net.ErrClosed) || len(raw.calls) != 3 {
		t.Fatal("closed connection changed transport deadline state")
	}
}

func TestStdDescriptorControlDoesNotCallUserCodeWithoutADescriptor(t *testing.T) {
	first, second := net.Pipe()
	defer first.Close()
	defer second.Close()
	cause := errors.New("syscall descriptor unavailable")
	cases := []struct {
		name string
		conn *fdConn
		want error
	}{{"missing-stream", &fdConn{}, net.ErrClosed}, {"missing-packet", &fdConn{udp: &stdUDPState{}}, net.ErrClosed}, {"custom-stream", &fdConn{conn: first}, ErrUnsupported}, {"failed-syscall-connection", &fdConn{conn: &stdControlFailureConn{stdRegistrationConn: newStdRegistrationConn(0), cause: cause}}, cause}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			if err := tc.conn.Control(func(uintptr) {
				called = true
			}); !errors.Is(err, tc.want) || called {
				t.Fatalf("Control = %v, callback = %v", err, called)
			}
		})
	}
}

func TestStdPeerDeadlineOwnershipAndTerminalCancellation(t *testing.T) {
	peer := &fdConn{udp: &stdUDPState{svr: &fdConn{}}}
	if err := peer.SetDeadline(time.Time{}); err != nil || peer.udp.deadlines != nil {
		t.Fatal("clearing unused peer deadlines allocated timer state")
	}
	when := time.Now().Add(time.Hour)
	if err := peer.SetDeadline(when); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.stopChildDeadlines)
	state := peer.udp.deadlines
	if state == nil || state.read.timer == nil || state.write.timer == nil {
		t.Fatal("peer read/write deadlines were not independently armed")
	}
	if err := peer.SetReadDeadline(when.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if state.read.at.Equal(state.write.at) {
		t.Fatal("replacing one peer deadline changed the other direction")
	}
	if err := peer.SetDeadline(time.Time{}); err != nil || !state.read.at.IsZero() || !state.write.at.IsZero() {
		t.Fatal("peer deadline cancellation did not clear both directions")
	}
	peer.stopChildDeadlines()
	if err := peer.SetWriteDeadline(when); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stopped peer accepted a new deadline: %v", err)
	}
}

func TestStdQueuedStringCopiesDataAndKeepsClosedAdmission(t *testing.T) {
	conn := queuedStdConnection(t, 64)
	if n, err := conn.WriteString("string"); n != 6 || err != nil {
		t.Fatalf("WriteString = %d, %v", n, err)
	}
	if n, err := conn.WriteString(""); n != 0 || err != nil {
		t.Fatal("empty string was not a no-op")
	}
	var got bytes.Buffer
	_, _ = conn.outbound.WriteTo(&got)
	if got.String() != "string" {
		t.Fatalf("queued string = %q", got.String())
	}
	conn.closing.Store(true)
	if err := conn.WriteByte('x'); !errors.Is(err, net.ErrClosed) || conn.OutboundBuffered() != 0 {
		t.Fatal("closed byte write changed the queue")
	}
}

func TestStdFinalCallbackSeesTerminalStateAndCannotRestartIO(t *testing.T) {
	states := make(chan ConnState, 1)
	events := &Events{Pollers: 1, Handler: FuncHandler{Close: func(c Conn, _ error) {
		states <- c.State()
	}}}
	_, conn := readControlServe(t, events)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if state := readControlAwait(t, states, "the final callback's state"); state&(StateClosing|StateClosed) != StateClosing|StateClosed {
		t.Fatalf("final connection state = %b", state)
	}
	if err := conn.Flush(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed flush = %v", err)
	}
	if err := conn.ReserveInbound(0); err != nil {
		t.Fatalf("empty input reservation = %v", err)
	}
}

func TestStdLargeWriteBudgetRejectsWholeInputBeforeCopyingIntoQueue(t *testing.T) {
	conn := queuedStdConnection(t, stdOwnedWriteThreshold)
	if n, err := conn.Write(make([]byte, stdOwnedWriteThreshold+1)); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("oversized write = %d, %v", n, err)
	}
	if _, err := conn.Write([]byte("accepted")); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{'x'}, stdOwnedWriteThreshold)
	if n, err := conn.Write(payload); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("large write beyond remaining capacity = %d, %v", n, err)
	}
	var got bytes.Buffer
	_, _ = conn.outbound.WriteTo(&got)
	if got.String() != "accepted" || !bytes.Equal(payload, bytes.Repeat([]byte{'x'}, len(payload))) {
		t.Fatal("refused large input changed the accepted prefix or caller data")
	}
}

func TestStdTimerErrorClosesBeforeAnEmptyReadCallback(t *testing.T) {
	cause := errors.New("application timer failed")
	closed := make(chan error, 1)
	var reads atomic.Int64
	events := &Events{Pollers: 1, Handler: FuncHandler{Timer: func(Conn) error {
		return cause
	}, Read: func(Conn) error {
		reads.Add(1)
		return nil
	}, Close: func(_ Conn, err error) {
		closed <- err
	}}}
	_, conn := readControlServe(t, events)
	if err := conn.SetWakeDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := readControlAwait(t, closed, "a failing timer's close"); !errors.Is(err, cause) || reads.Load() != 0 {
		t.Fatalf("timer close = %v; later read callbacks = %d", err, reads.Load())
	}
}

type stdHalfCloseFailureConn struct {
	*stdRegistrationConn
	cause  error
	closes atomic.Int64
}

func (c *stdHalfCloseFailureConn) CloseWrite() error {
	c.closes.Add(1)
	return c.cause
}

func TestStdHalfCloseSupportAndTransportFailureRemainDistinct(t *testing.T) {
	if err := (&fdConn{conn: newStdRegistrationConn(0)}).CloseWrite(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("custom transport without half-close = %v", err)
	}
	cause := errors.New("transport half-close failed")
	raw := &stdHalfCloseFailureConn{stdRegistrationConn: newStdRegistrationConn(30114), cause: cause}
	closed := make(chan error, 1)
	events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(Conn) {
		raw.openReturned.Store(true)
	}, Close: func(_ Conn, err error) {
		closed <- err
	}}}
	done := startStdTestEvents(t, events)
	t.Cleanup(func() {
		stopStdTestEvents(t, events, done, nil)
	})
	conn, err := events.Adopt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := readControlAwait(t, closed, "failed transport half-close"); !errors.Is(err, cause) || raw.closes.Load() != 1 {
		t.Fatalf("half-close cause = %v, calls = %d", err, raw.closes.Load())
	}
}
