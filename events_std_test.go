//go:build windows || stdio

package uio

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func BenchmarkStdWritev1MiB(b *testing.B) {
	conn := &fdConn{commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: -1}).plainEntry()}, writeSig: make(chan struct{}, 1)}
	header := make([]byte, 10)
	payload := make([]byte, 1<<20)
	vec := [][]byte{header, payload}
	wireSize := len(header) + len(payload)
	b.ReportAllocs()
	b.SetBytes(int64(wireSize))
	b.ResetTimer()
	for range b.N {
		if n, err := conn.Writev(vec); err != nil || n != wireSize {
			b.Fatalf("Writev() = %d, %v", n, err)
		}
		conn.mux.Lock()
		conn.outbound.Reset()
		conn.mux.Unlock()
	}
}

func BenchmarkStdConcurrentWritev64KiB(b *testing.B) {
	header := make([]byte, 10)
	payload := make([]byte, 64<<10)
	vec := [][]byte{header, payload}
	wireSize := len(header) + len(payload)
	for _, benchmark := range []struct {
		name  string
		write func(*fdConn)
	}{{name: "owned-outside-lock", write: func(conn *fdConn) {
		if n, err := conn.Writev(vec); err != nil || n != wireSize {
			panic("Writev failed")
		}
		conn.mux.Lock()
		conn.outbound.Reset()
		conn.mux.Unlock()
	}}, {name: "copy-under-lock", write: func(conn *fdConn) {
		conn.mux.Lock()
		_, _ = conn.outbound.Writev(vec)
		conn.outbound.Reset()
		conn.mux.Unlock()
	}}} {
		b.Run(benchmark.name, func(b *testing.B) {
			conn := &fdConn{commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: -1}).plainEntry()}, writeSig: make(chan struct{}, 1)}
			b.ReportAllocs()
			b.SetBytes(int64(wireSize))
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					benchmark.write(conn)
				}
			})
		})
	}
}

func BenchmarkStdWriteOwned1MiB(b *testing.B) {
	conn := &fdConn{commonConn: commonConn{entry: (&Events{MaxOutboundBuffered: -1}).plainEntry()}, writeSig: make(chan struct{}, 1)}
	payload := make([]byte, 1<<20)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		buffer := AcquireBuffer(len(payload))
		_, _ = buffer.Write(payload)
		if n, err := conn.WriteOwned(buffer); err != nil || n != len(payload) {
			b.Fatalf("WriteOwned() = %d, %v", n, err)
		}
		conn.mux.Lock()
		conn.outbound.Reset()
		conn.mux.Unlock()
		select {
		case <-conn.writeSig:
		default:
		}
	}
}

func TestStdFlushCoalescesWakeWithoutAllocating(t *testing.T) {
	conn := &fdConn{writeSig: make(chan struct{}, 1)}
	conn.entry = (&Events{}).plainEntry()
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(conn.writeSig) != 1 {
		t.Fatal("Flush did not signal the write loop")
	}
	var flushErr error
	allocations := testing.AllocsPerRun(1000, func() {
		flushErr = conn.Flush()
	})
	if flushErr != nil {
		t.Fatal(flushErr)
	}
	if allocations != 0 {
		t.Fatalf("Flush allocations = %.2f, want 0", allocations)
	}
	if len(conn.writeSig) != 1 {
		t.Fatal("Flush did not coalesce write-loop wakeups")
	}
}

func TestStdUDPOnOutboundCallsDoNotOverlap(t *testing.T) {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := net.DialUDP("udp", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var in, overlap, calls atomic.Int32
	var reported atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	events := &Events{}
	funcs(events).BytesWritten = func(_ Conn, n int) {
		if in.Add(1) > 1 {
			overlap.Add(1)
		}
		reported.Add(int64(n))
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		in.Add(-1)
	}
	conn := &fdConn{udp: &stdUDPState{sock: client}}
	conn.entry = events.plainEntry()
	firstDone := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("first"))
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("OnWrite did not run for the first datagram")
	}
	if _, err := conn.Write([]byte("second!")); err != nil {
		t.Fatalf("second Write error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("OnWrite ran %d times while the first call was still running", got)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Write error = %v", err)
	}
	if got := reported.Load(); got != int64(len("first")+len("second!")) {
		t.Fatalf("OnWrite reported %d bytes, want %d", got, len("first")+len("second!"))
	}
	var sent atomic.Int64
	var wg sync.WaitGroup
	for range // Many senders at once: the reports never overlap and add up to what was sent.
	4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if n, err := conn.Write([]byte("burst")); err == nil {
					sent.Add(int64(n))
				}
			}
		}()
	}
	wg.Wait()
	if n := overlap.Load(); n != 0 {
		t.Fatalf("OnWrite overlapped %d times for one UDP connection", n)
	}
	if want := int64(len("first")+len("second!")) + sent.Load(); reported.Load() != want {
		t.Fatalf("OnWrite reported %d bytes, want %d", reported.Load(), want)
	}
}

func TestStdHeldReadDeadlineAgreesWithTheSocket(t *testing.T) {
	const limit = 16 << 10
	var consume atomic.Bool
	closed := make(chan error, 1)
	events := withFuncs(&Events{Pollers: 1, ReadBufferSize: 4 << 10, MaxInboundBuffered: limit}, FuncHandler{Read: func(c Conn) error {
		if consume.Load() {
			_, _ = c.Discard(-1)
		}
		return nil
	}, Close: func(_ Conn, err error) {
		signal(closed, err)
	}})
	client, server := serveInboundTest(t, events)
	writeUntilStalled(t, client, 16<<20)
	paused, release := make(chan struct{}), make(chan struct{})
	var stopped atomic.Bool
	readDeadlineNotedForTest = func() {
		if stopped.CompareAndSwap(false, true) {
			close(paused)
			<-release
		}
	}
	t.Cleanup(func() {
		readDeadlineNotedForTest = nil
	})
	first := make(chan error, 1)
	go func() {
		first <- server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	}()
	<-paused
	second := make(chan error, 1)
	go func() {
		second <- server.SetReadDeadline(time.Time{})
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	expiredHeld := false
	select {
	case <-closed:
		expiredHeld = true
	case <-time.After(300 * time.Millisecond):
	}
	if expiredHeld {
		return
	}
	consume.Store(true)
	if err := server.Wake(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		t.Fatalf("the held wait had no deadline, but the resumed socket read expired: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestStdHeldDeadlineExpiryAndClearAreOrdered(t *testing.T) {
	const limit = 16 << 10
	closed := make(chan error, 1)
	events := withFuncs(&Events{Pollers: 1, ReadBufferSize: 4 << 10, MaxInboundBuffered: limit}, FuncHandler{Read: func(Conn) error {
		return nil
	}, Close: func(_ Conn, err error) {
		signal(closed, err)
	}})
	client, server := serveInboundTest(t, events)
	writeUntilStalled(t, client, 16<<20)
	paused, release := make(chan struct{}), make(chan struct{})
	var stopped atomic.Bool
	heldDeadlineExpiredForTest = func() {
		if stopped.CompareAndSwap(false, true) {
			close(paused)
			<-release
		}
	}
	t.Cleanup(func() {
		heldDeadlineExpiredForTest = nil
	})
	if err := server.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-paused:
	case <-time.After(2 * time.Second):
		t.Fatal("the held wait never found its deadline passed")
	}
	cleared := make(chan error, 1)
	go func() {
		cleared <- server.SetReadDeadline(time.Time{})
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-cleared; err != nil {
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("clear error = %v, want success or net.ErrClosed", err)
		}
		return
	}
	select {
	case err := <-closed:
		t.Fatalf("the clear succeeded, but the old deadline closed the connection: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestStdDialCanceledDuringRegistrationGetsNoCallbacks(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}()
		}
	}()
	var opens, closes atomic.Int32
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Open: func(Conn) {
		opens.Add(1)
	}, Close: func(Conn, error) {
		closes.Add(1)
	}})
	serveDone := startStdTestEvents(t, events)
	cause := errors.New("dial canceled while registering")
	ctx, cancel := context.WithCancelCause(context.Background())
	returned := make(chan struct{})
	registeredForTest = func(*fdConn) {
		cancel(cause)
		<-returned
	}
	t.Cleanup(func() {
		registeredForTest = nil
	})
	conn, err := events.DialContext(ctx, "tcp://"+target.Addr().String())
	close(returned)
	if conn != nil || !errors.Is(err, cause) {
		t.Fatalf("DialContext = %v, %v; want nil, %v", conn, err, cause)
	}
	stopStdTestEvents(t, events, serveDone, errors.New("test done"))
	if o, c := opens.Load(), closes.Load(); o != 0 || c != 0 {
		t.Fatalf("the abandoned connection got %d OnOpen and %d OnClose, want none", o, c)
	}
}

const stdioBackend = true

func TestStdWakeThenDialDoesNotStall(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		peer, err := target.Accept()
		if err == nil {
			defer peer.Close()
			var probe [1]byte
			_, _ = peer.Read(probe[:])
		}
	}()
	ev := &Events{Pollers: 1}
	started := make(chan struct{})
	result := make(chan error, 1)
	var calls atomic.Int32
	funcs(ev).Start = func(*Events) {
		close(started)
	}
	funcs(ev).Read = func(conn Conn) error {
		if calls.Add(1) != 1 {
			return nil
		}
		_, _ = conn.Discard(-1)
		if err := conn.Wake(); err != nil {
			result <- err
			return err
		}
		time.Sleep(50 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		dialed, err := ev.DialContext(ctx, target.Addr().String())
		if dialed != nil {
			_ = dialed.Close()
		}
		result <- err
		return nil
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- ev.Serve()
	}()
	<-started
	t.Cleanup(func() {
		_ = ev.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	client, server := tcpConnectionPair(t)
	defer client.Close()
	if _, err := ev.Adopt(server); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("DialContext from OnRead stalled behind queued Wake: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnRead failed to return")
	}
}

func TestStdUDPListenerCloseDuringDatagramFlood(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		started := make(chan net.Addr, 1)
		events := &Events{}
		funcs(events).Start = func(ev *Events) {
			for _, l := range ev.acceptor.listeners {
				if l.udp != nil {
					started <- l.udp.LocalAddr()
					return
				}
			}
		}
		funcs(events).Read = func(c Conn) error {
			return nil
		}
		serveDone := make(chan error, 1)
		go func() {
			serveDone <- events.Serve("udp://127.0.0.1:0")
		}()
		addr := <-started
		var stop atomic.Bool
		floodDone := make(chan struct{})
		go func() {
			defer close(floodDone)
			client, err := net.Dial("udp", addr.String())
			if err != nil {
				return
			}
			defer client.Close()
			for !stop.Load() {
				_, _ = client.Write([]byte("x"))
			}
		}()
		time.Sleep(2 * time.Millisecond)
		_ = events.Close(nil)
		<-serveDone
		stop.Store(true)
		<-floodDone
	}
}

func TestStdUDPChildClosedBeforeOpenGetsOnClose(t *testing.T) {
	started := make(chan net.Addr, 1)
	created := make(chan *fdConn, 1)
	release := make(chan struct{})
	testHookUDPChildCreated = func(c *fdConn) {
		created <- c
		<-release
	}
	defer func() {
		testHookUDPChildCreated = nil
	}()
	var opens, closes atomic.Int32
	events := &Events{}
	funcs(events).Start = func(ev *Events) {
		for _, l := range ev.acceptor.listeners {
			if l.udp != nil {
				started <- l.udp.LocalAddr()
				return
			}
		}
	}
	funcs(events).Open = func(c Conn) {
		opens.Add(1)
	}
	funcs(events).Close = func(c Conn, err error) {
		closes.Add(1)
	}
	funcs(events).Read = func(c Conn) error {
		return nil
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- events.Serve("udp://127.0.0.1:0")
	}()
	addr := <-started
	client, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = client.Write([]byte("x"))
	child := <-created
	_ = events.Close(nil)
	for !child.IsClosed() {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-serveDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	if opens.Load() != closes.Load() {
		t.Fatalf("UDP child got %d OnOpen and %d OnClose", opens.Load(), closes.Load())
	}
}

func TestUDPControlDuringListenerCloseIsRefused(t *testing.T) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, Hooks: FuncHandler{Start: func(*Events) {
		close(started)
	}}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	listener, err := events.Listen("udp4://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := listener.(PacketListener).DialPeer(netip.MustParseAddrPort("127.0.0.1:9001"), nil)
	if err != nil {
		t.Fatal(err)
	}
	server := listener.(packetListener).group.members[0].udpSvr
	server.mux.Lock()
	firstDone := make(chan struct{})
	go func() {
		_ = listener.Close()
		close(firstDone)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		stacks := string(buf[:runtime.Stack(buf, true)])
		if strings.Contains(stacks, "(*fdConn).closeOnLoop") && strings.Contains(stacks, "(*fdConn).closeListenerSocket") {
			break
		}
		if time.Now().After(deadline) {
			server.mux.Unlock()
			t.Fatal("the first Close never reached the socket's close")
		}
		runtime.Gosched()
	}
	controlled := make(chan error, 1)
	go func() {
		sc, _ := SocketOf(peer)
		controlled <- sc.Control(func(uintptr) {
			_ = listener.Close()
		})
	}()
	select {
	case err := <-controlled:
		if !errors.Is(err, net.ErrClosed) {
			server.mux.Unlock()
			t.Fatalf("a Control during the close = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		server.mux.Unlock()
		t.Fatal("a Control during the close neither ran nor was refused")
	}
	server.mux.Unlock()
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the first Close never finished")
	}
}

func TestAdoptContextCanceledWhileRegistering(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	events := &Events{Pollers: 1, Hooks: FuncHandler{Start: func(*Events) {
		close(started)
	}}}
	count := func(c Conn) {
		if c.Userdata() == "canceled" {
			calls.Add(1)
		}
	}
	events.Handler = FuncHandler{Open: count, Close: func(c Conn, _ error) {
		count(c)
	}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	pair := func() (net.Conn, net.Conn) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		remote, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		local, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = remote.Close()
		})
		return local, remote
	}
	local, _ := pair()
	blocker := &closeBlocker{Conn: local, entered: make(chan struct{}), release: make(chan struct{})}
	first, err := events.Adopt(blocker, WithLayers(PassThroughLayer))
	if err != nil {
		t.Fatal(err)
	}
	blocker.block.Store(true)
	_ = first.Abort(errors.New("abort"))
	<-blocker.entered
	second, peer := pair()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	adopted := make(chan error, 1)
	go func() {
		_, err := events.AdoptContext(ctx, second, WithLayers(PassThroughLayer), WithUserdata("canceled"))
		adopted <- err
	}()
	var adoptErr error
	select {
	case adoptErr = <-adopted:
	case <-time.After(500 * time.Millisecond):
		close(blocker.release)
		<-adopted
		t.Fatal("AdoptContext went on waiting past its deadline for the loop")
	}
	close(blocker.release)
	if !errors.Is(adoptErr, context.DeadlineExceeded) {
		t.Fatalf("AdoptContext = %v, want the deadline", adoptErr)
	}
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the connection the canceled adopt took was not closed: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("a canceled adopt ran %d callbacks", n)
	}
}

type closeBlocker struct {
	net.Conn
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *closeBlocker) SyscallConn() (syscall.RawConn, error) {
	return c.Conn.(syscall.Conn).SyscallConn()
}

func (c *closeBlocker) Close() error {
	if c.block.Load() {
		c.once.Do(func() {
			close(c.entered)
		})
		<-c.release
	}
	return c.Conn.Close()
}
