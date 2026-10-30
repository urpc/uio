/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package umux

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// metaHandler implements MetadataHandler: it records each metadata delivery
// without copying, so the tests see exactly the slice the session passed.
type metaHandler struct {
	mu     sync.Mutex
	calls  int
	meta   []byte
	opened chan *Stream
}

func (h *metaHandler) OnMetadata(_ uio.Conn, meta []byte) {
	h.mu.Lock()
	h.calls++
	h.meta = meta
	h.mu.Unlock()
}

func (h *metaHandler) OnOpen(c uio.Conn) { h.opened <- c.(*Stream) }

func (h *metaHandler) OnRead(c uio.Conn) error {
	st := c.(*Stream)
	buf := make([]byte, 4<<10)
	n, err := st.Read(buf)
	if n > 0 {
		_, _ = st.Write(buf[:n])
	}
	return err
}

func (h *metaHandler) OnTimer(uio.Conn) error  { return nil }
func (h *metaHandler) OnClose(uio.Conn, error) {}

func (h *metaHandler) lastMeta() ([]byte, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.meta, h.calls
}

// TestMetadataRoundTrip: the bytes attached to the SYN reach OnMetadata
// unchanged, whatever they contain, and only metadata opens call it.
func TestMetadataRoundTrip(t *testing.T) {
	h := &metaHandler{opened: make(chan *Stream, 4)}
	sess, _ := startPair(t, Config{Handler: h}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Binary, not text: NULs, an invalid UTF-8 byte, and a length no string
	// API would preserve.
	meta := []byte{0x00, 0xff, 'v', 0x00, '1', 0x80, 0x01}
	st, err := sess.OpenWith(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer st.CloseWith(nil)

	select {
	case <-h.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("server never opened the stream")
	}
	if got, calls := h.lastMeta(); !bytes.Equal(got, meta) || calls != 1 {
		t.Fatalf("OnMetadata saw %x after %d calls, want %x once", got, calls, meta)
	}

	if n, werr := st.Write([]byte("ping")); n != 4 || werr != nil {
		t.Fatalf("write = %d, %v", n, werr)
	}
	if got := string(readFull(t, st, 4)); got != "ping" {
		t.Fatalf("echo = %q, want %q", got, "ping")
	}

	// A plain open never reaches OnMetadata.
	plain, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.CloseWith(nil)
	select {
	case <-h.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("server never opened the plain stream")
	}
	if _, calls := h.lastMeta(); calls != 1 {
		t.Fatalf("OnMetadata ran %d times, want only the metadata open", calls)
	}
}

// TestMetadataDroppedWithoutCapability: a handler without the capability
// still accepts metadata streams; the bytes are simply not delivered.
func TestMetadataDroppedWithoutCapability(t *testing.T) {
	h := &drainHandler{opened: make(chan *Stream, 1)}
	sess, _ := startPair(t, Config{Handler: h}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := sess.OpenWith(ctx, []byte("route=a"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.CloseWith(nil)
	select {
	case <-h.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("server never opened the stream")
	}
}

// TestMetadataPullMode: the Accepted value carries the metadata and the
// session keeps no other reference.
func TestMetadataPullMode(t *testing.T) {
	s, _ := recPair(Config{})
	s.acceptCh = make(chan Accepted, 1)
	payload := append([]byte{0, 0, 0x10, 0}, []byte("route=a")...)
	if err := s.handleSynMetadata(frameHeader{typ: typeSynMetadata, flags: flagSYN, streamID: 3, length: uint32(len(payload))}, payload); err != nil {
		t.Fatal(err)
	}
	var in Accepted
	select {
	case in = <-s.acceptCh:
	default:
		t.Fatal("stream never reached the accept queue")
	}
	if in.Stream == nil || string(in.Metadata) != "route=a" {
		t.Fatalf("accepted %+v, want the stream with metadata %q", in, "route=a")
	}
}

// TestMetadataCopiedOutOfTransport: what the session hands over is its own
// copy, so the transport reusing the read buffer cannot rewrite it.
func TestMetadataCopiedOutOfTransport(t *testing.T) {
	h := &metaHandler{opened: make(chan *Stream, 1)}
	s, _ := recPair(Config{Handler: h})
	payload := append([]byte{0, 0, 0x10, 0}, []byte("route=a")...)
	if err := s.handleSynMetadata(frameHeader{typ: typeSynMetadata, flags: flagSYN, streamID: 3, length: uint32(len(payload))}, payload); err != nil {
		t.Fatal(err)
	}
	<-h.opened
	copy(payload[4:], "xxxxxxx") // the transport reuses its buffer
	if got, _ := h.lastMeta(); string(got) != "route=a" {
		t.Fatalf("metadata = %q after the transport reused its buffer, want %q", got, "route=a")
	}
}

// metaWire records both headers and payloads of the frames a session writes.
type metaWire struct {
	*quietWire
	mu       sync.Mutex
	payloads [][]byte
}

// Write records control frames; flushLocked writes them as byte slices.
func (c *metaWire) Write(p []byte) (int, error) {
	if len(p) >= headerSize {
		if h, err := decodeHeader(p[:headerSize]); err == nil {
			c.quietWire.mu.Lock()
			c.quietWire.frames = append(c.quietWire.frames, h)
			c.quietWire.mu.Unlock()
		}
	}
	c.mu.Lock()
	c.payloads = append(c.payloads, append([]byte(nil), p...))
	c.mu.Unlock()
	return len(p), nil
}

func (c *metaWire) WriteOwned(b *uio.Buffer) (int, error) {
	c.mu.Lock()
	c.payloads = append(c.payloads, append([]byte(nil), b.Bytes()...))
	c.mu.Unlock()
	return c.quietWire.WriteOwned(b)
}

func (c *metaWire) payloadSnap() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.payloads...)
}

// TestMetadataWireShape: an open without metadata stays a yamux SYN, and an
// open with metadata is the extension frame that carries the window delta
// followed by the bytes untouched.
func TestMetadataWireShape(t *testing.T) {
	wire := &metaWire{quietWire: &quietWire{}}
	s := &Session{
		cfg:      Config{KeepAlive: -1, StreamOpenTimeout: 20 * time.Millisecond}.resolve(),
		conn:     wire,
		nextID:   2, // server-parity session: local streams are even
		streams:  make(map[uint32]*Stream),
		openWait: make(map[uint32]chan error),
	}
	// No ACK ever comes: Open abandons the stream when its ctx ends, and the
	// frames it wrote are on the wire.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.open(ctx, nil); err == nil {
		t.Fatal("plain open succeeded without an ACK")
	}
	// The abandoned open's cleanup RST may follow the SYN; the SYN leads.
	frames := wire.seen()
	if len(frames) == 0 || frames[0].typ != typeWindowUpdate || frames[0].flags != flagSYN || frames[0].streamID != 2 {
		t.Fatalf("plain open wrote %+v, want a WindowUpdate SYN first", frames)
	}

	meta := []byte{0x00, 0xff, 0x01}
	if _, err := s.OpenWith(ctx, meta); err == nil {
		t.Fatal("metadata open succeeded without an ACK")
	}
	var payload []byte
	var got frameHeader
	for _, p := range wire.payloadSnap() {
		h, derr := decodeHeader(p[:headerSize])
		if derr == nil && h.typ == typeSynMetadata {
			payload, got = p, h
			break
		}
	}
	if payload == nil {
		t.Fatalf("metadata open wrote no metadata SYN: %+v", wire.seen())
	}
	if got.flags != flagSYN || got.streamID != 4 || got.length != 4+uint32(len(meta)) {
		t.Fatalf("metadata open wrote %+v, want a SYN-flagged metadata frame on stream 4", got)
	}
	wantDelta := uint32(s.cfg.WindowSize - minWindow)
	if delta := uint32(payload[headerSize])<<24 | uint32(payload[headerSize+1])<<16 | uint32(payload[headerSize+2])<<8 | uint32(payload[headerSize+3]); delta != wantDelta {
		t.Fatalf("payload delta = %d, want %d", delta, wantDelta)
	}
	if !bytes.Equal(payload[headerSize+4:], meta) {
		t.Fatalf("payload metadata = %x, want %x", payload[headerSize+4:], meta)
	}
}

// TestMetadataTooLargeLocally: metadata past the local cap is refused before
// anything reaches the wire.
func TestMetadataTooLargeLocally(t *testing.T) {
	s, wire := recPair(Config{MaxMetadata: 64})
	before := wire.deadlineCount()
	if _, err := s.OpenWith(context.Background(), make([]byte, 65)); !errors.Is(err, ErrMetadataTooLarge) {
		t.Fatalf("open = %v, want ErrMetadataTooLarge", err)
	}
	if wire.deadlineCount() != before || s.Closed() {
		t.Fatal("a refused OpenWith touched the session")
	}
}

// TestMetadataRejectedByPeer: a receiver with a smaller cap closes the session
// on the oversized SYN, and the opener's Open fails instead of hanging.
func TestMetadataRejectedByPeer(t *testing.T) {
	sess, _ := startPair(t,
		Config{Handler: &metaHandler{opened: make(chan *Stream, 4)}, MaxMetadata: 64},
		Config{MaxMetadata: 1024})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sess.OpenWith(ctx, make([]byte, 100)); err == nil {
		t.Fatal("OpenWith succeeded past the receiver's MaxMetadata")
	}
}

// TestTransportLimitFitsMetadataFrame: the transport's inbound bound must hold
// one whole metadata SYN even when the session budget is smaller, or readFrame
// would wait for a frame that can never fit.
func TestTransportLimitFitsMetadataFrame(t *testing.T) {
	cfg := Config{
		ReadBufferSize: 4 << 10,
		SessionWindow:  64 << 10,
		WindowSize:     256 << 10,
		MaxMetadata:    1 << 20,
	}.resolve()
	if got, want := transportReadLimit(cfg), (1<<20)+headerSize+4; got < want {
		t.Fatalf("transport read limit = %d, want at least one %d-byte metadata frame", got, want)
	}
}

// TestLargeMetadataUnderSmallTransportBudget: end to end, a metadata SYN
// larger than the session budget still arrives whole.
func TestLargeMetadataUnderSmallTransportBudget(t *testing.T) {
	h := &metaHandler{opened: make(chan *Stream, 1)}
	sess, _ := startPair(t,
		Config{Handler: h, ReadBufferSize: 4 << 10, SessionWindow: 64 << 10, MaxMetadata: 1 << 20},
		Config{MaxMetadata: 1 << 20})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	meta := bytes.Repeat([]byte{0xab}, 300<<10)
	st, err := sess.OpenWith(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer st.CloseWith(nil)
	select {
	case <-h.opened:
	case <-time.After(10 * time.Second):
		t.Fatal("server never opened the stream: the metadata frame never fit the transport")
	}
	got, _ := h.lastMeta()
	if !bytes.Equal(got, meta) {
		t.Fatalf("OnMetadata saw %d bytes, want %d", len(got), len(meta))
	}
}
