package uio

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// halfCloseTestData is a payload larger than the socket buffers, so a FIN
// sent before it drained would cut it short.
func halfCloseTestData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*29 + i>>11)
	}
	return data
}

// readAllWithin reads until the end of the stream, failing on a timeout.
func readAllWithin(t *testing.T, conn net.Conn, d time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read to the end of stream after %d bytes: %v", len(got), err)
	}
	return got
}

// CloseWrite sends the end of the stream only after everything written
// before it, and the connection keeps reading: the peer still gets its
// writes through, and its own end of stream closes the connection.
func TestCloseWriteFollowsQueuedOutput(t *testing.T) {
	want := halfCloseTestData(4 << 20)
	var mu sync.Mutex
	var got bytes.Buffer
	closed := make(chan error, 1)
	events := &Events{
		Pollers: 1,
		OnOpen: func(c Conn) {
			if _, err := c.Write(want); err != nil {
				t.Errorf("write: %v", err)
			}
			if err := c.CloseWrite(); err != nil {
				t.Errorf("CloseWrite: %v", err)
			}
		},
		OnData: func(c Conn) error {
			mu.Lock()
			defer mu.Unlock()
			_, err := c.WriteTo(&got)
			return err
		},
		OnClose: func(_ Conn, err error) { closed <- err },
	}
	client, _ := serveInboundTest(t, events)
	time.Sleep(50 * time.Millisecond) // let the output back up behind the FIN
	if got := readAllWithin(t, client, 10*time.Second); !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes before the end of stream, want %d intact", len(got), len(want))
	}
	if _, err := client.Write([]byte("after")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		s := got.String()
		mu.Unlock()
		if s == "after" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server read %q after its CloseWrite, want %q", s, "after")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = client.Close()
	select {
	case err := <-closed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("close error = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the peer's end of stream did not close the connection")
	}
}

// After CloseWrite every write is refused, from a callback and from another
// goroutine, and a second CloseWrite does nothing.
func TestWritesAfterCloseWriteAreRefused(t *testing.T) {
	results := make(chan error, 8)
	events := &Events{
		Pollers: 1,
		OnData: func(c Conn) error {
			_, _ = c.Discard(-1)
			if err := c.CloseWrite(); err != nil {
				results <- err
				return nil
			}
			_, err := c.Write([]byte("x"))
			results <- err
			_, err = c.Writev([][]byte{[]byte("y")})
			results <- err
			buf := AcquireBuffer(1)
			buf.CommitWrite(copy(buf.AvailableBuffer()[:1], "z"))
			if _, err = c.WriteOwned(buf); err == ErrOutboundOverflow {
				ReleaseBuffer(buf)
			}
			results <- err
			_, err = c.ReserveOutbound(1)
			if errors.Is(err, ErrReserveUnsupported) {
				err = net.ErrClosed // the stdio backend has no reservations
			}
			results <- err
			results <- c.CloseWrite()
			return nil
		},
	}
	client, server := serveInboundTest(t, events)
	if _, err := client.Write([]byte("go")); err != nil {
		t.Fatal(err)
	}
	for i, want := range []error{net.ErrClosed, net.ErrClosed, net.ErrClosed, net.ErrClosed, nil} {
		select {
		case err := <-results:
			if !errors.Is(err, want) && !(want == nil && err == nil) {
				t.Fatalf("call %d after CloseWrite = %v, want %v", i, err, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("OnData did not run")
		}
	}
	if _, err := server.Write([]byte("outside")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write from another goroutine after CloseWrite = %v, want net.ErrClosed", err)
	}
	if got := readAllWithin(t, client, 2*time.Second); len(got) != 0 {
		t.Fatalf("the peer read %q after CloseWrite", got)
	}
}

// With AllowHalfOpen, the end of the peer's stream leaves the connection
// open: OnData runs once more and Read reports io.EOF, the connection still
// writes its reply, and CloseWrite then closes it.
func TestAllowHalfOpenKeepsWritingAfterPeerEOF(t *testing.T) {
	reply := halfCloseTestData(1 << 20)
	var request bytes.Buffer
	closed := make(chan error, 1)
	events := &Events{
		Pollers:       1,
		AllowHalfOpen: true,
		OnData: func(c Conn) error {
			buf := make([]byte, 4096)
			for {
				n, err := c.Read(buf)
				request.Write(buf[:n])
				if err == io.EOF {
					if _, err := c.Write(reply); err != nil {
						return err
					}
					return c.CloseWrite()
				}
				if n == 0 {
					return err
				}
			}
		},
		OnClose: func(_ Conn, err error) { closed <- err },
	}
	client, _ := serveInboundTest(t, events)
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := readAllWithin(t, client, 10*time.Second); !bytes.Equal(got, reply) {
		t.Fatalf("read %d reply bytes, want %d intact", len(got), len(reply))
	}
	select {
	case err := <-closed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("close error = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("both halves shut, but the connection did not close")
	}
	if request.String() != "request" {
		t.Fatalf("server read %q, want %q", request.String(), "request")
	}
}

// Without AllowHalfOpen the end of the peer's stream closes the connection
// as before, and Read never reports io.EOF.
func TestPeerEOFClosesWithoutAllowHalfOpen(t *testing.T) {
	var sawEOF atomic.Bool
	closed := make(chan error, 1)
	events := &Events{
		Pollers: 1,
		OnData: func(c Conn) error {
			if _, err := c.Read(make([]byte, 64)); err == io.EOF {
				sawEOF.Store(true)
			}
			return nil
		},
		OnClose: func(_ Conn, err error) { closed <- err },
	}
	client, _ := serveInboundTest(t, events)
	if _, err := client.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	_ = client.(*net.TCPConn).CloseWrite()
	select {
	case err := <-closed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("close error = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the end of the peer's stream did not close the connection")
	}
	if sawEOF.Load() {
		t.Fatal("Read reported io.EOF on a connection that does not allow half-open")
	}
}

// The tunnel shape: a relay forwards each direction to the other connection
// and passes each end of stream on with CloseWrite. A request that its
// client ends with a FIN gets its whole reply, and both legs close.
func TestHalfCloseThroughARelay(t *testing.T) {
	request := halfCloseTestData(2 << 20)
	reply := halfCloseTestData(3 << 20)[1:]

	sinkLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sinkLn.Close()
	sinkDone := make(chan error, 1)
	go func() {
		sink, err := sinkLn.Accept()
		if err != nil {
			sinkDone <- err
			return
		}
		defer sink.Close()
		got, err := io.ReadAll(sink)
		if err != nil || !bytes.Equal(got, request) {
			sinkDone <- errors.New("the sink did not get the whole request before its end")
			return
		}
		if _, err := sink.Write(reply); err != nil {
			sinkDone <- err
			return
		}
		sinkDone <- sink.(*net.TCPConn).CloseWrite()
	}()

	type leg struct{ peer atomic.Value } // Conn
	var closes atomic.Int32
	events := &Events{Pollers: 1, AllowHalfOpen: true}
	forward := func(c Conn) error {
		l, _ := c.Userdata().(*leg)
		if l == nil {
			return nil
		}
		peer, _ := l.peer.Load().(Conn)
		if peer == nil {
			return nil // the other leg is not up yet; Wake forwards later
		}
		if _, err := c.WriteTo(peer); err != nil {
			return err
		}
		if _, err := c.Read(nil); err == io.EOF {
			return peer.CloseWrite()
		}
		return nil
	}
	events.OnData = forward
	events.OnClose = func(Conn, error) { closes.Add(1) }
	client, in := serveInboundTest(t, events)
	inLeg := &leg{}
	in.SetUserdata(inLeg)
	outLeg := &leg{}
	outLeg.peer.Store(in)
	out, err := events.Dial("tcp://"+sinkLn.Addr().String(), outLeg)
	if err != nil {
		t.Fatal(err)
	}
	inLeg.peer.Store(out)
	_ = in.Wake()

	go func() {
		_, _ = client.Write(request)
		_ = client.(*net.TCPConn).CloseWrite()
	}()
	if got := readAllWithin(t, client, 20*time.Second); !bytes.Equal(got, reply) {
		t.Fatalf("client read %d reply bytes, want %d intact", len(got), len(reply))
	}
	if err := <-sinkDone; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for closes.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("%d of the relay's two legs closed after both ends shut", closes.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A peer that shut its writing half and then resets is noticed on a
// half-open connection, although nothing reads it any more: on the native
// backend at once, through the hangup it raises; on the stdio backend, whose
// net.Conn reports nothing past the end of stream without a write, by the
// next write.
func TestAllowHalfOpenNoticesAResetAfterEOF(t *testing.T) {
	eof := make(chan struct{})
	closed := make(chan error, 1)
	events := &Events{
		Pollers:       1,
		AllowHalfOpen: true,
		OnData: func(c Conn) error {
			if _, err := c.Read(make([]byte, 64)); err == io.EOF {
				close(eof)
			}
			return nil
		},
		OnClose: func(_ Conn, err error) { closed <- err },
	}
	client, server := serveInboundTest(t, events)
	tcp := client.(*net.TCPConn)
	_ = tcp.CloseWrite()
	select {
	case <-eof:
	case <-time.After(2 * time.Second):
		t.Fatal("OnData did not see the end of stream")
	}
	select {
	case err := <-closed:
		t.Fatalf("closed at the peer's end of stream: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	_ = tcp.SetLinger(0)
	_ = tcp.Close() // a reset
	if stdioBackend {
		time.Sleep(100 * time.Millisecond)
		for range 3 { // the first write after a reset may still be accepted
			_, _ = server.Write([]byte("x"))
			time.Sleep(20 * time.Millisecond)
		}
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("a reset after the end of stream left the connection open")
	}
}

// A CloseWrite racing writes from another goroutine splits them cleanly:
// every write it accepted reaches the peer before the end of stream, and
// every write after it is refused.
func TestCloseWriteRacingWritesKeepsTheStreamWhole(t *testing.T) {
	// The outbound limit bounds the queue the writer fills, so a regression
	// that kept accepting writes would spin here, not exhaust memory.
	events := &Events{Pollers: 1, MaxOutboundBuffered: 4 << 20}
	client, server := serveInboundTest(t, events)
	var accepted atomic.Int64
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		chunk := make([]byte, 1024)
		stop := time.Now().Add(5 * time.Second)
		for seq := 0; ; {
			if time.Now().After(stop) {
				t.Error("writes were still accepted 5s after CloseWrite")
				return
			}
			for i := range chunk {
				chunk[i] = byte(seq)
			}
			n, err := server.Write(chunk)
			accepted.Add(int64(n))
			if errors.Is(err, ErrOutboundOverflow) {
				time.Sleep(time.Millisecond) // the peer drains the queue
				continue
			}
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("write %d: %v", seq, err)
				}
				return
			}
			seq++
		}
	}()
	// The peer checks the stream as it reads, keeping none of it.
	readDone := make(chan error, 1)
	var read int64
	go func() {
		buf := make([]byte, 64<<10)
		_ = client.SetReadDeadline(time.Now().Add(20 * time.Second))
		for {
			n, err := client.Read(buf)
			for i := range n {
				if pos := read + int64(i); buf[i] != byte(pos/1024) {
					readDone <- errors.New("the stream was reordered")
					return
				}
			}
			read += int64(n)
			if err == io.EOF {
				readDone <- nil
				return
			}
			if err != nil {
				readDone <- err
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if err := server.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	<-writerDone
	if err := <-readDone; err != nil {
		t.Fatalf("after %d bytes: %v", read, err)
	}
	if read != accepted.Load() {
		t.Fatalf("peer read %d bytes, %d were accepted", read, accepted.Load())
	}
}

// Bytes a callback reserved before its CloseWrite go out before the end of
// stream.
func TestCloseWriteAfterReserveOutbound(t *testing.T) {
	want := halfCloseTestData(300)
	events := &Events{
		Pollers: 1,
		OnData: func(c Conn) error {
			_, _ = c.Discard(-1)
			dst, err := c.ReserveOutbound(len(want))
			if errors.Is(err, ErrReserveUnsupported) {
				_, err = c.Write(want) // the stdio backend has no reservations
			} else if err == nil {
				copy(dst, want)
			}
			if err != nil {
				return err
			}
			return c.CloseWrite()
		},
	}
	client, _ := serveInboundTest(t, events)
	if _, err := client.Write([]byte("go")); err != nil {
		t.Fatal(err)
	}
	if got := readAllWithin(t, client, 5*time.Second); !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes before the end of stream, want the %d reserved", len(got), len(want))
	}
}

// A peer that ends its stream while reads are held at MaxInboundBuffered:
// the end of stream waits behind the held bytes, and once a Wake consumes
// them OnData sees Read report io.EOF and the connection stays open.
func TestAllowHalfOpenEOFBehindHeldReads(t *testing.T) {
	const limit = 16 << 10
	var consume atomic.Bool
	var received atomic.Int64
	sawEOF := make(chan struct{})
	var once sync.Once
	closed := make(chan error, 1)
	events := &Events{
		Pollers:            1,
		MaxBufferSize:      4 << 10,
		MaxInboundBuffered: limit,
		AllowHalfOpen:      true,
		OnData: func(c Conn) error {
			if !consume.Load() {
				return nil
			}
			buf := make([]byte, 8<<10)
			for {
				n, err := c.Read(buf)
				received.Add(int64(n))
				if err == io.EOF {
					once.Do(func() { close(sawEOF) })
					return nil
				}
				if n == 0 {
					return err
				}
			}
		},
		OnClose: func(_ Conn, err error) { closed <- err },
	}
	client, server := serveInboundTest(t, events)
	const total = 256 << 10
	go func() {
		_, _ = client.Write(make([]byte, total))
		_ = client.(*net.TCPConn).CloseWrite()
	}()
	time.Sleep(200 * time.Millisecond)
	consume.Store(true)
	if err := server.Wake(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sawEOF:
	case <-time.After(5 * time.Second):
		t.Fatalf("no end of stream after consuming; received %d of %d", received.Load(), total)
	}
	if got := received.Load(); got != total {
		t.Fatalf("received %d of %d bytes before the end of stream", got, total)
	}
	select {
	case err := <-closed:
		t.Fatalf("a half-open connection closed at the peer's end of stream: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := server.Write([]byte("still open")); err != nil {
		t.Fatalf("write after the peer's end of stream: %v", err)
	}
}

// A datagram connection has no writing half to shut.
func TestCloseWriteUnsupportedOnUDP(t *testing.T) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.LocalAddr().String()
	_ = probe.Close()
	started := make(chan struct{})
	events := &Events{Pollers: 1, OnStart: func(*Events) { close(started) }}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	<-started
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	conn, err := events.Dial("udp://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err == nil {
		t.Fatal("CloseWrite on a UDP connection succeeded")
	}
}
