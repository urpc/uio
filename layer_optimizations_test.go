package uio

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// optimizationSocket keeps the layer tests independent of network timing.
// Each write is atomic, as a socket Conn's Write and Writev must be.
type optimizationSocket struct {
	Conn
	userdata any
	deadline time.Time
	queue    int
	mu       sync.Mutex
	written  []byte
	writes   int
	vectors  int
	segments int
	short    bool
}

func (s *optimizationSocket) LocalAddr() net.Addr               { return nil }
func (s *optimizationSocket) Userdata() any                     { return s.userdata }
func (s *optimizationSocket) SetUserdata(v any)                 { s.userdata = v }
func (s *optimizationSocket) SetWakeDeadline(t time.Time) error { s.deadline = t; return nil }
func (s *optimizationSocket) OutboundBuffered() int             { return s.queue }
func (s *optimizationSocket) Wake() error                       { return nil }
func (s *optimizationSocket) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.short && len(p) > 0 {
		p = p[:len(p)-1]
	}
	s.written = append(s.written, p...)
	return len(p), nil
}
func (s *optimizationSocket) Writev(vec [][]byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vectors++
	s.segments += len(vec)
	n := 0
	for _, p := range vec {
		n += len(p)
		s.written = append(s.written, p...)
	}
	if s.short && n > 0 {
		s.written = s.written[:len(s.written)-1]
		n--
	}
	return n, nil
}

func optimizationStack(handlers ...LayerHandler) (*stack, *optimizationSocket) {
	socket := &optimizationSocket{}
	layers := make([]Layer, len(handlers))
	for i, handler := range handlers {
		layers[i] = LayerFunc(func(LayerContext) LayerHandler { return handler })
	}
	(&stackDriver{layers: layers, top: FuncHandler{}}).OnOpen(socket)
	return socket.userdata.(*stack), socket
}

type optimizationCapability struct {
	PassThrough
	index int
	call  func(string, int)
}

func (h optimizationCapability) OnWireSent(LayerContext, int) { h.call("sent", h.index) }
func (h optimizationCapability) Flush(LayerContext) error {
	h.call("flush", h.index)
	return nil
}

func TestLayerCapabilityCallbacksKeepStackOrder(t *testing.T) {
	for _, mask := range []int{0, 1, 5, 7} {
		t.Run(fmt.Sprintf("capabilities=%03b", mask), func(t *testing.T) {
			var calls []string
			handlers := make([]LayerHandler, 3)
			for i := range handlers {
				handlers[i] = passThroughHandler{}
				if mask&(1<<uint(i)) != 0 {
					handlers[i] = optimizationCapability{index: i, call: func(kind string, index int) {
						calls = append(calls, fmt.Sprintf("%s%d", kind, index))
					}}
				}
			}
			c, _ := optimizationStack(handlers...)
			calls = nil
			c.sent(3)
			if err := c.flushHeld(); err != nil {
				t.Fatal(err)
			}
			var want []string
			for i := 0; i < 3; i++ {
				if mask&(1<<uint(i)) != 0 {
					want = append(want, fmt.Sprintf("sent%d", i))
				}
			}
			for i := 2; i >= 0; i-- {
				if mask&(1<<uint(i)) != 0 {
					want = append(want, fmt.Sprintf("flush%d", i))
				}
			}
			if fmt.Sprint(calls) != fmt.Sprint(want) {
				t.Fatalf("callbacks = %v, want %v", calls, want)
			}
		})
	}
}

func TestLayerTimerActiveLifecycle(t *testing.T) {
	c, socket := optimizationStack(passThroughHandler{})
	past := time.Now().Add(-time.Second)
	future := time.Now().Add(time.Hour)
	for _, slot := range []int{0, 1} {
		t.Run(fmt.Sprintf("slot=%d", slot), func(t *testing.T) {
			if err := c.setTimer(slot, future, 0); err != nil {
				t.Fatal(err)
			}
			if !c.hasTimers.Load() || !socket.deadline.Equal(future) {
				t.Fatal("arming a timer did not reactivate its socket deadline")
			}
			if err := c.setTimer(slot, time.Time{}, 0); err != nil {
				t.Fatal(err)
			}
			if c.hasTimers.Load() || !socket.deadline.IsZero() || c.takeDue() != 0 {
				t.Fatal("cancelling the last timer left the timer path active")
			}
			if err := c.setTimer(slot, past, 0); err != nil {
				t.Fatal(err)
			}
			if due := c.takeDue(); due != 1<<uint(slot) {
				t.Fatalf("rearmed timer due mask = %b, want %b", due, 1<<uint(slot))
			}
			if c.hasTimers.Load() || !socket.deadline.IsZero() {
				t.Fatal("firing the last one-shot left the timer path active")
			}
			if due := c.takeDue(); due != 0 {
				t.Fatalf("one-shot fired again: %b", due)
			}
		})
	}
	if err := c.setTimer(0, past, time.Hour); err != nil {
		t.Fatal(err)
	}
	if due := c.takeDue(); due != 1 || !c.hasTimers.Load() || socket.deadline.IsZero() {
		t.Fatalf("repeating timer lost its next wake: due=%b active=%v deadline=%v", due, c.hasTimers.Load(), socket.deadline)
	}
	c.stopTimers()
	if c.hasTimers.Load() {
		t.Fatal("closed stack retained its active timer flag")
	}
}

func TestLayerTimerAttachedBeforeOpenStaysActive(t *testing.T) {
	socket := &optimizationSocket{}
	var attached *stack
	past := time.Now().Add(-time.Second)
	layers := []Layer{
		LayerFunc(func(ctx LayerContext) LayerHandler {
			attached = ctx.(layerCtx).l.stack
			if err := ctx.SetTimer(past); err != nil {
				t.Fatal(err)
			}
			if due := attached.takeDue(); due != 0 || !attached.hasTimers.Load() || !socket.deadline.IsZero() {
				t.Fatalf("unopened timer: due=%b active=%v deadline=%v", due, attached.hasTimers.Load(), socket.deadline)
			}
			return passThroughHandler{}
		}),
	}
	(&stackDriver{layers: layers, top: FuncHandler{}}).OnOpen(socket)
	if !socket.deadline.Equal(past) {
		t.Fatal("opening did not arm the timer installed by Attach")
	}
	if due := attached.takeDue(); due != 1 || attached.hasTimers.Load() {
		t.Fatalf("attached one-shot: due=%b active=%v", due, attached.hasTimers.Load())
	}
}

type optimizationHolder struct {
	PassThrough
	index int
	read  func(int) int
}

func (h optimizationHolder) OutboundHeld(LayerContext) int { return h.read(h.index) }

func TestLayerQueueQueriesKeepHolderOrder(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprintf("holders=%03b", mask), func(t *testing.T) {
			var reads []int
			closed := false
			handlers := make([]LayerHandler, 3)
			for i := range handlers {
				handlers[i] = passThroughHandler{}
				if mask&(1<<uint(i)) != 0 {
					handlers[i] = optimizationHolder{index: i, read: func(i int) int {
						if closed {
							t.Fatal("closed stack called a layer's OutboundHeld")
						}
						reads = append(reads, i)
						return i + 1
					}}
				}
			}
			c, socket := optimizationStack(handlers...)
			socket.queue = 17
			c.wireQueued.Store(100)
			for i, node := range c.layers {
				held := 0
				var want []int
				for j := i; j >= 0; j-- {
					if mask&(1<<uint(j)) != 0 {
						held += j + 1
						want = append(want, j)
					}
				}
				reads = nil
				if got := node.OutboundBuffered(); got != 17+held || fmt.Sprint(reads) != fmt.Sprint(want) {
					t.Fatalf("node %d outbound=%d reads=%v, want %d and %v", i, got, reads, 17+held, want)
				}
				reads = nil
				for left, right := 0, len(want)-1; left < right; left, right = left+1, right-1 {
					want[left], want[right] = want[right], want[left]
				}
				if got := node.WireQueued(); got != uint64(100+held) || fmt.Sprint(reads) != fmt.Sprint(want) {
					t.Fatalf("node %d wire=%d reads=%v, want %d and %v", i, got, reads, 100+held, want)
				}
			}
			closed = true
			c.markClosed()
			for _, node := range c.layers {
				if got := node.OutboundBuffered(); got != 17 {
					t.Fatalf("closed outbound=%d, want socket queue 17", got)
				}
				if got := node.WireQueued(); got != 100 {
					t.Fatalf("closed wire=%d, want socket position 100", got)
				}
			}
		})
	}
}

// Reading the socket before a holder can miss bytes during a transfer down.
func TestLayerQueueReadsHoldBeforeSocket(t *testing.T) {
	var held atomic.Int64
	held.Store(90)
	lower := &scriptedQueue{read: make(chan struct{}), goOn: make(chan struct{})}
	c := &stack{socket: lower, holders: true, firstHeld: 0}
	node := &layerNode{stack: c, lower: lower, ops: layerOps{held: func(LayerContext) int {
		v := int(held.Load())
		lower.once.Do(func() { close(lower.read) })
		<-lower.goOn
		return v
	}}}
	c.layers = []*layerNode{node}
	got := make(chan int, 1)
	go func() { got <- node.OutboundBuffered() }()
	<-lower.read
	lower.queue.Store(90)
	held.Store(0)
	close(lower.goOn)
	if buffered := <-got; buffered < 90 {
		t.Fatalf("bytes disappeared during transfer: %d", buffered)
	}
}

type optimizationTransform struct {
	PassThrough
	calls int
	input []byte
}

func (h *optimizationTransform) OnWrite(ctx LayerContext, p []byte) (int, error) {
	h.calls++
	h.input = append(h.input[:0], p...)
	return ctx.Down().Write(bytes.ToUpper(p))
}

func TestLayerWritevKeepsEmbeddedTransform(t *testing.T) {
	transform := &optimizationTransform{}
	c, socket := optimizationStack(passThroughHandler{}, transform, passThroughHandler{})
	if n, err := c.topConn().Writev([][]byte{[]byte("a"), []byte("bc")}); n != 3 || err != nil {
		t.Fatalf("Writev = %d, %v", n, err)
	}
	if transform.calls != 1 || string(transform.input) != "abc" || string(socket.written) != "ABC" {
		t.Fatalf("transform calls=%d input=%q wire=%q", transform.calls, transform.input, socket.written)
	}
}

func TestLayerBuiltinWritevKeepsSingleAndEmptyCalls(t *testing.T) {
	for _, vec := range [][][]byte{nil, {}, {[]byte("one")}} {
		c, socket := optimizationStack(passThroughHandler{}, passThroughHandler{})
		if _, err := c.topConn().Writev(vec); err != nil {
			t.Fatal(err)
		}
		if socket.writes != 1 || socket.vectors != 0 {
			t.Fatalf("vec=%q writes=%d vectors=%d, want one OnWrite", vec, socket.writes, socket.vectors)
		}
	}
}

func TestLayerBuiltinWritevEmptySegmentsKeepAdmission(t *testing.T) {
	vec := [][]byte{nil, {}}
	transform := &optimizationTransform{}
	c, socket := optimizationStack(passThroughHandler{}, transform, passThroughHandler{})
	if n, err := c.topConn().Writev(vec); n != 0 || err != nil || transform.calls != 1 || socket.writes != 1 {
		t.Fatalf("empty transformed Writev = %d, %v, calls=%d writes=%d", n, err, transform.calls, socket.writes)
	}
	c.limit = 1
	socket.queue = 2
	if n, err := c.topConn().Writev(vec); n != 0 || !errors.Is(err, ErrOutboundOverflow) || transform.calls != 1 {
		t.Fatalf("empty overflow Writev = %d, %v, calls=%d", n, err, transform.calls)
	}
	c.markClosed()
	if n, err := c.topConn().Writev(vec); n != 0 || !errors.Is(err, net.ErrClosed) || transform.calls != 1 {
		t.Fatalf("empty closed Writev = %d, %v, calls=%d", n, err, transform.calls)
	}
}

func TestLayerBuiltinWritevAdmissionAndShortWrite(t *testing.T) {
	vec := [][]byte{[]byte("a"), []byte("bc")}
	c, socket := optimizationStack(passThroughHandler{}, passThroughHandler{})
	c.limit = 2
	if n, err := c.topConn().Writev(vec); n != 0 || !errors.Is(err, ErrOutboundOverflow) || socket.vectors != 0 {
		t.Fatalf("overflow Writev = %d, %v, vectors=%d", n, err, socket.vectors)
	}
	c.limit = 0
	socket.short = true
	if n, err := c.topConn().Writev(vec); n != 2 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short Writev = %d, %v", n, err)
	}
	c.markClosed()
	if n, err := c.topConn().Writev(vec); n != 0 || !errors.Is(err, net.ErrClosed) || socket.vectors != 1 {
		t.Fatalf("closed Writev = %d, %v, vectors=%d", n, err, socket.vectors)
	}
}

func TestLayerBuiltinWritevWholeAcrossLayers(t *testing.T) {
	const writers, each, size = 4, 300, 8
	c, socket := optimizationStack(passThroughHandler{}, passThroughHandler{}, passThroughHandler{})
	var wg sync.WaitGroup
	errors := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mark := byte('A' + w)
			vec := [][]byte{{mark}, bytes.Repeat([]byte{mark}, size-1)}
			for range each {
				if n, err := c.topConn().Writev(vec); n != size || err != nil {
					errors <- fmt.Errorf("Writev = %d, %v", n, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if len(socket.written) != writers*each*size || socket.vectors != writers*each || socket.segments != writers*each*2 || socket.writes != 0 {
		t.Fatalf("wire bytes=%d vectors=%d segments=%d single writes=%d", len(socket.written), socket.vectors, socket.segments, socket.writes)
	}
	for i := 0; i < len(socket.written); i += size {
		if !bytes.Equal(socket.written[i:i+size], bytes.Repeat(socket.written[i:i+1], size)) {
			t.Fatalf("vector at %d torn: %q", i, socket.written[i:i+size])
		}
	}
}

type optimizationTimerSocket struct {
	optimizationSocket
	deadlineMu sync.Mutex
}

func (s *optimizationTimerSocket) SetWakeDeadline(t time.Time) error {
	s.deadlineMu.Lock()
	s.deadline = t
	s.deadlineMu.Unlock()
	return nil
}

func (s *optimizationTimerSocket) wakeDeadline() time.Time {
	s.deadlineMu.Lock()
	defer s.deadlineMu.Unlock()
	return s.deadline
}

func TestLayerTimerConcurrentCancelRearm(t *testing.T) {
	socket := &optimizationTimerSocket{}
	(&stackDriver{layers: []Layer{PassThroughLayer}, top: FuncHandler{}}).OnOpen(socket)
	c := socket.userdata.(*stack)
	stop := make(chan struct{})
	done := make(chan struct{})
	checkpoint := make(chan chan struct{})
	fired := make(chan struct{}, 1)
	failures := make(chan error, 1)
	report := func(err error) {
		select {
		case failures <- err:
		default:
		}
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case ack := <-checkpoint:
				close(ack)
			default:
			}
			if c.takeDue()&1 != 0 {
				select {
				case fired <- struct{}{}:
				default:
				}
			}
			c.reaim()
			c.timerMu.Lock()
			active := false
			for _, timer := range c.timers {
				active = active || !timer.when.IsZero()
			}
			if got := c.hasTimers.Load(); got != active {
				report(fmt.Errorf("timer guard=%v with active slots=%v", got, active))
			}
			c.timerMu.Unlock()
		}
	}()
	defer func() { close(stop); <-done }()

	var writers sync.WaitGroup
	for slot := range 2 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := range 300 {
				when := time.Now().Add(time.Hour)
				if i%3 == 0 {
					when = time.Now().Add(-time.Second)
				}
				if err := c.setTimer(slot, when, 0); err != nil {
					report(err)
					return
				}
				if i%2 == 0 {
					if err := c.setTimer(slot, time.Time{}, 0); err != nil {
						report(err)
						return
					}
				}
			}
		}()
	}
	writers.Wait()
	for slot := range 2 {
		if err := c.setTimer(slot, time.Time{}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if c.hasTimers.Load() || !socket.wakeDeadline().IsZero() {
		t.Fatal("cancelling the final timers did not stop the timer path")
	}

	// Wait until the reader has finished all earlier due reports, then
	// rearm from the inactive state while it continues takeDue and reaim.
	ack := make(chan struct{})
	checkpoint <- ack
	<-ack
	select {
	case <-fired:
	default:
	}
	if err := c.setTimer(0, time.Now().Add(-time.Second), 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(layerWait):
		t.Fatal("timer rearmed after the last cancel was never delivered")
	}
	if c.hasTimers.Load() || !socket.wakeDeadline().IsZero() {
		t.Fatal("the final rearmed one-shot did not retire")
	}
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
}

// Independent audit: a PacketLayer can delegate its unchanged handler to
// the exported built-in layer. Its upper Conn still carries a byte stream.
type regressionPacketPass struct{}

func (regressionPacketPass) CarriesPackets() {}
func (regressionPacketPass) Attach(ctx LayerContext) LayerHandler {
	return PassThroughLayer.Attach(ctx)
}

type regressionDatagramSocket struct {
	optimizationSocket
	datagrams [][]byte
}

func (s *regressionDatagramSocket) LocalAddr() net.Addr { return &net.UDPAddr{} }
func (s *regressionDatagramSocket) Write(p []byte) (int, error) {
	s.datagrams = append(s.datagrams, append([]byte(nil), p...))
	return len(p), nil
}
func (s *regressionDatagramSocket) Writev(v [][]byte) (int, error) {
	n := 0
	for _, p := range v {
		if len(p) > 0 {
			m, _ := s.Write(p)
			n += m
		}
	}
	return n, nil
}
func TestLayerRegressionPacketPassWritevDatagramBoundary(t *testing.T) {
	socket := &regressionDatagramSocket{}
	(&stackDriver{layers: []Layer{regressionPacketPass{}}, top: FuncHandler{}}).OnOpen(socket)
	c := socket.userdata.(*stack)
	n, err := c.topConn().Writev([][]byte{[]byte("a"), []byte("bc")})
	if n != 3 || err != nil {
		t.Fatalf("writev=(%d,%v)", n, err)
	}
	if len(socket.datagrams) != 1 || string(socket.datagrams[0]) != "abc" {
		t.Fatalf("datagrams=%q, want one abc datagram", socket.datagrams)
	}
}

type regressionSentOnWriteSocket struct{ optimizationSocket }

func (s *regressionSentOnWriteSocket) Write(p []byte) (int, error) {
	n, err := s.optimizationSocket.Write(p)
	if n > 0 {
		stackSent(nil)(s.userdata.(*stack).topConn(), n)
	}
	return n, err
}

type regressionAttachSendObserver struct {
	PassThrough
	sent *int
}

func (h regressionAttachSendObserver) OnWireSent(_ LayerContext, n int) { *h.sent += n }
func TestLayerRegressionAttachWritesKeepSendReports(t *testing.T) {
	s := &regressionSentOnWriteSocket{}
	heard := 0
	layers := []Layer{
		LayerFunc(func(ctx LayerContext) LayerHandler {
			ctx.Up().Open()
			return regressionAttachSendObserver{sent: &heard}
		}),
		LayerFunc(func(ctx LayerContext) LayerHandler {
			if n, err := ctx.Down().Write([]byte("x")); n != 1 || err != nil {
				t.Fatalf("attach write=(%d,%v)", n, err)
			}
			return PassThrough{}
		}),
	}
	(&stackDriver{layers: layers, top: FuncHandler{}}).OnOpen(s)
	if heard != 1 {
		t.Fatalf("observer heard %d bytes, want 1", heard)
	}
}

func TestLayerRegressionPacketPassWritevUDP(t *testing.T) {
	writeResult := make(chan error, 1)
	_, addr := servePacketLayer(t, "udp", FuncHandler{
		Open: func(c Conn) {
			_, err := c.Writev([][]byte{[]byte("a"), []byte("bc")})
			writeResult <- err
		},
	}, regressionPacketPass{})
	peer, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	_ = peer.SetDeadline(time.Now().Add(time.Second))
	if _, err = peer.Write([]byte("open")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 32)
	n, err := peer.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b[:n]); got != "abc" {
		t.Fatalf("first UDP datagram=%q, want abc", got)
	}
	if err = <-writeResult; err != nil {
		t.Fatal(err)
	}
}

// The lower holder's methods and ops are fully attached before the upper
// Attach launches a permitted queue reader. Only firstHeld is still built.
func TestLayerRegressionWireQueuedDuringAttach(t *testing.T) {
	s := &optimizationSocket{}
	readerStop := make(chan struct{})
	readerDone := make(chan struct{})
	readerStarted := make(chan struct{})
	layers := []Layer{
		LayerFunc(func(LayerContext) LayerHandler {
			return optimizationHolder{read: func(int) int { return 7 }}
		}),
		LayerFunc(func(ctx LayerContext) LayerHandler {
			counter := ctx.Down().(WireCounter)
			go func() {
				defer close(readerDone)
				close(readerStarted)
				for {
					select {
					case <-readerStop:
						return
					default:
					}
					if got := counter.WireQueued(); got != 7 {
						t.Errorf("WireQueued during Attach=%d, want 7", got)
						return
					}
				}
			}()
			<-readerStarted
			return PassThrough{}
		}),
	}
	(&stackDriver{layers: layers, top: FuncHandler{}}).OnOpen(s)
	close(readerStop)
	<-readerDone
}

func TestLayerRegressionPacketPassWritevAcrossTransparentChain(t *testing.T) {
	for _, depth := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("depth=%d", depth), func(t *testing.T) {
			layers := []Layer{regressionPacketPass{}}
			for len(layers) < depth {
				layers = append(layers, PassThroughLayer)
			}
			s := &regressionDatagramSocket{}
			(&stackDriver{layers: layers, top: FuncHandler{}}).OnOpen(s)
			c := s.userdata.(*stack)
			n, err := c.topConn().Writev([][]byte{[]byte("a"), nil, {}, []byte("bc")})
			if n != 3 || err != nil {
				t.Fatalf("writev=(%d,%v)", n, err)
			}
			if len(s.datagrams) != 1 || string(s.datagrams[0]) != "abc" {
				t.Fatalf("datagrams=%q, want one abc", s.datagrams)
			}
		})
	}
}

type regressionAttachOrderedObserver struct {
	PassThrough
	index int
	calls *[]string
}

func (h regressionAttachOrderedObserver) OnWireSent(_ LayerContext, n int) {
	*h.calls = append(*h.calls, fmt.Sprintf("%d:%d", h.index, n))
}
func TestLayerRegressionAttachSendObserversKeepBottomUpOrder(t *testing.T) {
	s := &regressionSentOnWriteSocket{}
	var calls []string
	layers := make([]Layer, 3)
	for index := range layers {
		layers[index] = LayerFunc(func(ctx LayerContext) LayerHandler {
			ctx.Up().Open()
			if index > 0 {
				payload := []byte("xyz")[:index]
				if n, err := ctx.Down().Write(payload); n != index || err != nil {
					t.Fatalf("attach write=(%d,%v), want %d", n, err, index)
				}
				if len(calls) != 0 {
					t.Fatalf("observer ran inside Attach: %v", calls)
				}
			}
			return regressionAttachOrderedObserver{index: index, calls: &calls}
		})
	}
	(&stackDriver{layers: layers, top: FuncHandler{}}).OnOpen(s)
	if got := fmt.Sprint(calls); got != "[0:3 1:3 2:3]" {
		t.Fatalf("send reports=%s, want [0:3 1:3 2:3]", got)
	}
}
