package umux

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

func TestSessionProtocolRejectionsDoNotPublishStreams(t *testing.T) {
	s, st, _, h := productionStreamPair(t)
	for _, frame := range []frameHeader{
		{typ: typeData, flags: flagSYN, streamID: st.id},
		{typ: typeSynMetadata, streamID: 2},
	} {
		if err := s.handleFrame(frame, nil, false); !errors.Is(err, ErrProtocol) {
			t.Fatalf("invalid frame type=%d flags=%d: %v", frame.typ, frame.flags, err)
		}
	}
	for _, id := range []uint32{0, 3} {
		s.client = true // the remote server can open only nonzero even IDs
		if err := s.checkPeerStreamID(id); !errors.Is(err, ErrProtocol) {
			t.Fatalf("invalid server stream ID %d: %v", id, err)
		}
	}
	if err := s.handleFrame(frameHeader{typ: 255}, nil, false); err != nil || !s.goAway {
		t.Fatal("unknown extension did not stop new stream admission")
	}
	if err := s.handleWindowUpdate(frameHeader{streamID: st.id}); err != nil {
		t.Fatal("an empty credit update was not harmless")
	}
	if err := s.handleData(frameHeader{streamID: st.id, flags: flagRST}, nil, false); err != nil {
		t.Fatal(err)
	}
	s.drainCloses()
	if closed := h.closed(); len(closed) != 1 || !errors.Is(closed[0], ErrStreamReset) {
		t.Fatalf("data-frame reset closes = %v", closed)
	}
	if len(s.streams) != 0 {
		t.Fatal("reset stream remained published")
	}
	s.closed = true
	if err := s.acceptStream(2, 0, nil); err != nil || len(s.streams) != 0 {
		t.Fatal("late SYN published a stream after transport shutdown")
	}
}

func TestSessionShutdownCallsShareOneDrainAndPreserveItsResult(t *testing.T) {
	s, st, _, _ := productionStreamPair(t)
	results := make(chan error, 2)
	var started sync.WaitGroup
	for range 2 {
		started.Add(1)
		go func() {
			started.Done()
			results <- s.Shutdown(nil)
		}()
	}
	started.Wait()
	waitFor(t, "shutdown admission barrier", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.draining
	})
	s.abortStream(st, ErrStreamReset)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("shared graceful shutdown = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("one of the shutdown callers missed the drain completion")
		}
	}
	s.forceDrain(context.Canceled) // a late cancellation cannot change success
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("completed drain lost its result: %v", err)
	}
}

func TestSessionShutdownReportsBrokenTransportAndAlreadyClosedCause(t *testing.T) {
	cause := errors.New("test: GoAway write failed")
	wire := &productionRejectWire{writeErr: cause}
	s := newSession(wire, Config{KeepAlive: -1}.resolve(), true)
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[st.id] = st
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- s.Shutdown(ctx) }()
	waitFor(t, "broken transport close", wire.closed.Load)
	s.onTransportClose(cause)
	if err := <-result; !errors.Is(err, cause) {
		t.Fatalf("shutdown = %v, want broken transport cause", err)
	}
	closed := newSession(&productionSessionWire{}, Config{KeepAlive: -1}.resolve(), true)
	closed.onTransportClose(cause)
	if err := closed.Shutdown(nil); !errors.Is(err, cause) {
		t.Fatalf("shutdown after transport failure = %v", err)
	}
}

func TestSessionRendezvousAndAnnouncementAfterServerClose(t *testing.T) {
	wire := &productionSessionWire{}
	s := newSession(wire, Config{KeepAlive: -1}.resolve(), true)
	r := &router{conns: map[uio.Conn]*Session{wire: s}}
	if got, err := r.waitSession(nil, wire); err != nil || got != s {
		t.Fatal("completed session rendezvous did not return its existing session")
	}
	r.OnStop(nil)
	server := NewServer(Config{KeepAlive: -1})
	server.closeSessions()
	server.publishSession(s)
	if !wire.closed.Load() {
		t.Fatal("a session announced after shutdown was left live")
	}
	if _, ok := <-server.Sessions(); ok {
		t.Fatal("server shutdown reopened its announcement queue")
	}
}

// Opening or acknowledging a stream can also carry data and end its writing
// half. Conflicting lifecycle flags must still be rejected before dispatch.
func TestValidateFrameRejectsContradictoryFlagCombinations(t *testing.T) {
	const maxData, maxMeta = 256 << 10, 4 << 10
	bad := []struct {
		name string
		h    frameHeader
	}{
		{"data syn+ack", frameHeader{typ: typeData, flags: flagSYN | flagACK, streamID: 1}},
		{"data fin+rst", frameHeader{typ: typeData, flags: flagFIN | flagRST, streamID: 1}},
		{"data ack+rst", frameHeader{typ: typeData, flags: flagACK | flagRST, streamID: 1}},
		{"data on the session", frameHeader{typ: typeData, streamID: 0}},
		{"update syn+ack", frameHeader{typ: typeWindowUpdate, flags: flagSYN | flagACK, streamID: 1}},
		{"update fin+rst", frameHeader{typ: typeWindowUpdate, flags: flagFIN | flagRST, streamID: 1}},
		{"update on the session", frameHeader{typ: typeWindowUpdate, streamID: 0}},
		{"ping bare", frameHeader{typ: typePing}},
		{"ping fin", frameHeader{typ: typePing, flags: flagFIN}},
		{"ping syn+ack", frameHeader{typ: typePing, flags: flagSYN | flagACK}},
		{"goaway with flags", frameHeader{typ: typeGoAway, flags: flagACK}},
		{"goaway for a stream", frameHeader{typ: typeGoAway, streamID: 1}},
		{"metadata syn+ack", frameHeader{typ: typeSynMetadata, flags: flagSYN | flagACK, streamID: 1, length: 4}},
	}
	for _, tc := range bad {
		if err := validateFrame(tc.h, maxData, maxMeta); !errors.Is(err, errFrameFlags) {
			t.Errorf("%s: validateFrame = %v, want errFrameFlags", tc.name, err)
		}
	}
	good := []frameHeader{
		{typ: typeData, streamID: 1, length: 16},
		{typ: typeData, flags: flagFIN, streamID: 1},
		{typ: typeData, flags: flagRST, streamID: 1},
		{typ: typeData, flags: flagSYN, streamID: 1, length: 16},
		{typ: typeData, flags: flagSYN | flagFIN, streamID: 1, length: 16},
		// A yamux peer acknowledges the open and ends its half on the frame
		// that carries the data.
		{typ: typeData, flags: flagACK, streamID: 1, length: 16},
		{typ: typeData, flags: flagACK | flagFIN, streamID: 1, length: 16},
		{typ: typeWindowUpdate, streamID: 1, length: 4},
		{typ: typeWindowUpdate, flags: flagSYN, streamID: 1, length: 4},
		{typ: typeWindowUpdate, flags: flagACK, streamID: 1, length: 4},
		{typ: typeWindowUpdate, flags: flagSYN | flagFIN, streamID: 1, length: 4},
		{typ: typeWindowUpdate, flags: flagACK | flagFIN, streamID: 1, length: 4},
		{typ: typeWindowUpdate, flags: flagFIN, streamID: 1},
		{typ: typeWindowUpdate, flags: flagRST, streamID: 1},
		{typ: typePing, flags: flagSYN, length: 7},
		{typ: typePing, flags: flagACK, length: 7},
		{typ: typeGoAway},
		{typ: typeSynMetadata, flags: flagSYN, streamID: 1, length: 8},
	}
	for _, h := range good {
		if err := validateFrame(h, maxData, maxMeta); err != nil {
			t.Errorf("%+v: validateFrame = %v, want accepted", h, err)
		}
	}
}

// A frame the validation rejects ends the session with the frame error,
// before any handler runs: a peer cannot steer stream state with
// contradictory flags.
func TestSessionClosesOnContradictoryFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    frameHeader
	}{
		{"data fin+rst", frameHeader{typ: typeData, flags: flagFIN | flagRST, streamID: 1}},
		{"data syn+ack", frameHeader{typ: typeData, flags: flagSYN | flagACK, streamID: 1}},
		{"data ack+rst", frameHeader{typ: typeData, flags: flagACK | flagRST, streamID: 1}},
		{"update syn+ack", frameHeader{typ: typeWindowUpdate, flags: flagSYN | flagACK, streamID: 1}},
		{"ping bare", frameHeader{typ: typePing}},
		{"data on the session", frameHeader{typ: typeData, streamID: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			accepted := make(chan net.Conn, 1)
			go func() {
				if c, err := ln.Accept(); err == nil {
					accepted <- c
				}
			}()
			cli := NewClient(Config{})
			t.Cleanup(func() { _ = cli.Close(nil) })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sess, err := cli.Dial(ctx, "tcp://"+ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			transport := <-accepted
			t.Cleanup(func() { _ = transport.Close() })
			hb := make([]byte, headerSize)
			encodeHeader(hb, tc.h)
			if _, err := transport.Write(hb); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "session end on the invalid frame", sess.Closed)
			if cause := sess.Err(); !errors.Is(cause, errFrameFlags) {
				t.Fatalf("close cause = %v, want errFrameFlags", cause)
			}
		})
	}
}

// protocolRawPeer connects a pull-mode session to a peer whose frames the test
// controls. Every blocking network operation has a deadline.
func protocolRawPeer(t *testing.T) (*Session, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, err := ln.Accept()
		accepted <- acceptResult{conn, err}
	}()
	client := NewClient(Config{KeepAlive: -1})
	t.Cleanup(func() {
		_ = client.Close(nil)
		_ = client.Wait()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := client.Dial(ctx, "tcp://"+ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatal(result.err)
		}
		t.Cleanup(func() { _ = result.conn.Close() })
		return sess, result.conn
	case <-ctx.Done():
		t.Fatal("raw peer did not accept the transport")
		return nil, nil
	}
}

func protocolReadFrame(t *testing.T, peer net.Conn) (frameHeader, []byte) {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var header [headerSize]byte
	if _, err := io.ReadFull(peer, header[:]); err != nil {
		t.Fatalf("read peer frame: %v", err)
	}
	h, err := decodeHeader(header[:])
	if err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if h.typ == typeData || h.typ == typeSynMetadata {
		if h.length > 1<<20 {
			t.Fatalf("unexpected peer payload length %d", h.length)
		}
		payload = make([]byte, int(h.length))
		if _, err := io.ReadFull(peer, payload); err != nil {
			t.Fatalf("read peer payload: %v", err)
		}
	}
	return h, payload
}

func protocolWriteFrame(t *testing.T, peer net.Conn, h frameHeader, payload []byte) {
	t.Helper()
	if err := peer.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	frame := rawFramed(h.typ, h.flags, h.streamID, h.length, payload)
	if n, err := peer.Write(frame); err != nil || n != len(frame) {
		t.Fatalf("write peer frame = %d/%d, %v", n, len(frame), err)
	}
}

// protocolReplyToOpen observes the actual SYN from Session.Open before replying.
// An ACK on a data frame acknowledges the open; its length counts payload bytes,
// unlike the window delta on a WindowUpdate ACK.
func protocolReplyToOpen(t *testing.T, sess *Session, peer net.Conn, typ uint8, flags uint16, payload []byte) *Stream {
	t.Helper()
	type openResult struct {
		stream *Stream
		err    error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opened := make(chan openResult, 1)
	go func() {
		stream, err := sess.Open(ctx)
		opened <- openResult{stream, err}
	}()
	syn, _ := protocolReadFrame(t, peer)
	if syn.typ != typeWindowUpdate || syn.flags != flagSYN {
		t.Fatalf("open sent %+v, want WindowUpdate|SYN", syn)
	}
	length := uint32(0)
	if typ == typeData {
		length = uint32(len(payload))
	}
	protocolWriteFrame(t, peer, frameHeader{typ: typ, flags: flags, streamID: syn.streamID, length: length}, payload)
	select {
	case result := <-opened:
		if result.err != nil {
			t.Fatalf("Open after peer reply: %v", result.err)
		}
		if result.stream.ID() != syn.streamID {
			t.Fatalf("opened stream ID = %d, want %d", result.stream.ID(), syn.streamID)
		}
		return result.stream
	case <-ctx.Done():
		t.Fatal("Open did not finish after the peer acknowledged it")
		return nil
	}
}

func protocolAssertSecondStream(t *testing.T, sess *Session, peer net.Conn) {
	t.Helper()
	if sess.Closed() {
		t.Fatalf("session closed after a legal combined frame: %v", sess.Err())
	}
	second := protocolReplyToOpen(t, sess, peer, typeData, flagACK|flagFIN, []byte("second"))
	if got := string(readFull(t, second, len("second"))); got != "second" {
		t.Fatalf("second stream payload = %q", got)
	}
	waitEOF(t, second)
	// The peer's FIN ends only its writing half; the local half still works.
	writeAll(t, second, []byte("reply"))
	h, payload := protocolReadFrame(t, peer)
	if h.typ != typeData || h.streamID != second.ID() || string(payload) != "reply" {
		t.Fatalf("second stream write = %+v %q", h, payload)
	}
	if sess.Closed() {
		t.Fatalf("second stream ended the session: %v", sess.Err())
	}
}

func TestSessionAcceptsAckAndFinRidingOnData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     uint8
		flags   uint16
		payload string
	}{
		{"data_ack_empty", typeData, flagACK, ""},
		{"data_ack_payload", typeData, flagACK, "hello"},
		{"data_ack_fin_empty", typeData, flagACK | flagFIN, ""},
		{"data_ack_fin_payload", typeData, flagACK | flagFIN, "hello"},
		{"window_ack_fin", typeWindowUpdate, flagACK | flagFIN, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, peer := protocolRawPeer(t)
			st := protocolReplyToOpen(t, sess, peer, tc.typ, tc.flags, []byte(tc.payload))
			if got := string(readFull(t, st, len(tc.payload))); got != tc.payload {
				t.Fatalf("payload = %q, want %q", got, tc.payload)
			}
			if tc.flags&flagFIN == 0 {
				if st.State()&uio.StatePeerEnded != 0 {
					t.Fatal("an ACK without FIN ended the peer's half")
				}
				protocolWriteFrame(t, peer, frameHeader{typ: typeData, flags: flagFIN, streamID: st.ID()}, nil)
			}
			waitEOF(t, st)
			protocolAssertSecondStream(t, sess, peer)
		})
	}
}

func TestSessionAcceptsSynAndFinRidingOnData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     uint8
		flags   uint16
		payload string
	}{
		{"data_syn_payload", typeData, flagSYN, "hello"},
		{"data_syn_fin_empty", typeData, flagSYN | flagFIN, ""},
		{"data_syn_fin_payload", typeData, flagSYN | flagFIN, "hello"},
		{"window_syn_fin", typeWindowUpdate, flagSYN | flagFIN, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, peer := protocolRawPeer(t)
			length := uint32(0)
			if tc.typ == typeData {
				length = uint32(len(tc.payload))
			}
			// A client session accepts the server's even-numbered streams.
			protocolWriteFrame(t, peer, frameHeader{typ: tc.typ, flags: tc.flags, streamID: 2, length: length}, []byte(tc.payload))
			var st *Stream
			select {
			case accepted := <-sess.Accept():
				if accepted.Stream == nil || accepted.Stream.ID() != 2 {
					t.Fatalf("accepted stream = %+v", accepted)
				}
				st = accepted.Stream
			case <-time.After(5 * time.Second):
				t.Fatal("the peer's SYN did not publish its stream")
			}
			ack, _ := protocolReadFrame(t, peer)
			if ack.typ != typeWindowUpdate || ack.flags != flagACK || ack.streamID != st.ID() {
				t.Fatalf("accepted stream reply = %+v, want WindowUpdate|ACK", ack)
			}
			if got := string(readFull(t, st, len(tc.payload))); got != tc.payload {
				t.Fatalf("payload = %q, want %q", got, tc.payload)
			}
			if tc.flags&flagFIN == 0 {
				if st.State()&uio.StatePeerEnded != 0 {
					t.Fatal("a SYN without FIN ended the peer's half")
				}
				protocolWriteFrame(t, peer, frameHeader{typ: typeData, flags: flagFIN, streamID: st.ID()}, nil)
			}
			waitEOF(t, st)
			protocolAssertSecondStream(t, sess, peer)
		})
	}
}

// A window past what the wire carries clamps to the wire's bound instead of
// truncating into a smaller limit. Keep the oversized value representable in
// this test's source even when compiling it for a 32-bit target.
func TestWindowSizePastTheWireClamps(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("WindowSize cannot exceed the uint32 wire limit on a 32-bit target")
	}
	oversized := uint64(1) << 32
	window := int(oversized)
	cfg := Config{WindowSize: window}.resolve()
	if uint64(cfg.WindowSize) != oversized-1 {
		t.Fatalf("WindowSize = %d, want the clamp at %d", cfg.WindowSize, oversized-1)
	}
	sess, echo := startPair(t,
		Config{WindowSize: window, KeepAlive: -1},
		Config{WindowSize: window, KeepAlive: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writeAll(t, st, []byte("k"))
	if got := string(readFull(t, st, 1)); got != "k" {
		t.Fatalf("echo = %q, want k", got)
	}
	var remote *Stream
	select {
	case remote = <-echo.opened:
	case <-ctx.Done():
		t.Fatal("echo server did not open its stream")
	}
	waitFor(t, "echo server consumption", func() bool { return remote.InboundBuffered() == 0 })
	remote.sess.mu.Lock()
	pending := remote.sess.pendingBytes
	remote.sess.mu.Unlock()
	if pending != 0 {
		t.Fatalf("echo server retained %d bytes after consumption", pending)
	}
}
