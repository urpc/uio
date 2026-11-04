//go:build stdio && !windows

package uio

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urpc/uio/internal/poller"
)

type adoptionContractConn struct {
	*stdRegistrationConn
	syscallError error
	controlError error
	local        net.Addr
	onControl    func()
}

func (c *adoptionContractConn) LocalAddr() net.Addr {
	if c.local != nil {
		return c.local
	}
	return c.stdRegistrationConn.LocalAddr()
}
func (c *adoptionContractConn) SyscallConn() (syscall.RawConn, error) {
	if c.syscallError != nil {
		return nil, c.syscallError
	}
	return adoptionContractRaw{c}, nil
}

type adoptionContractRaw struct{ conn *adoptionContractConn }

func (r adoptionContractRaw) Control(fn func(uintptr)) error {
	if r.conn.controlError != nil {
		return r.conn.controlError
	}
	if r.conn.onControl != nil {
		r.conn.onControl()
	}
	fn(r.conn.fd)
	return nil
}
func (adoptionContractRaw) Read(func(uintptr) bool) error  { return ErrUnsupported }
func (adoptionContractRaw) Write(func(uintptr) bool) error { return ErrUnsupported }

func TestStdAdoptFailureAlwaysReleasesTransferredOwnership(t *testing.T) {
	ev := &Events{Pollers: 1, MaxBufferSize: 64}
	var opens atomic.Int64
	funcs(ev).Open = func(Conn) { opens.Add(1) }
	done := startStdTestEvents(t, ev)
	defer stopStdTestEvents(t, ev, done, nil)
	failure := errors.New("adopt boundary failure")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(failure)
	cases := []struct {
		name string
		raw  *adoptionContractConn
		opts []AdoptOption
		ctx  context.Context
		want error
	}{
		{"nil-layer", &adoptionContractConn{stdRegistrationConn: newStdRegistrationConn(53101)}, []AdoptOption{WithLayers(nil)}, context.Background(), nil},
		{"packet-layer", &adoptionContractConn{stdRegistrationConn: newStdRegistrationConn(53102), local: &net.UDPAddr{}}, []AdoptOption{WithLayers(PassThroughLayer)}, context.Background(), ErrLayersNeedStream},
		{"syscall-connection", &adoptionContractConn{stdRegistrationConn: newStdRegistrationConn(53103), syscallError: failure}, nil, context.Background(), ErrUnsupported},
		{"descriptor-control", &adoptionContractConn{stdRegistrationConn: newStdRegistrationConn(53104), controlError: failure}, nil, context.Background(), ErrUnsupported},
		{"invalid-descriptor", &adoptionContractConn{stdRegistrationConn: newStdRawRegistrationConn(^uintptr(0))}, nil, context.Background(), ErrUnsupported},
		{"cancelled-registration", &adoptionContractConn{stdRegistrationConn: newStdRegistrationConn(53105)}, nil, ctx, failure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ev.AdoptContext(tc.ctx, tc.raw, tc.opts...)
			if c != nil || err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("Adopt=%v,%v want %v", c, err, tc.want)
			}
			if tc.want == nil && !strings.Contains(err.Error(), "nil Layer") {
				t.Fatal(err)
			}
			select {
			case <-tc.raw.closed:
			default:
				t.Fatal("failed adoption retained the transport")
			}
		})
	}
	if c, err := ev.Adopt(nil); c != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nil adoption=%v,%v", c, err)
	}
	first, second := net.Pipe()
	defer second.Close()
	if c, err := ev.Adopt(first); c != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unowned descriptor=%v,%v", c, err)
	}
	if _, err := second.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("rejected pipe remained open: %v", err)
	}
	if opens.Load() != 0 {
		t.Fatal("failed adoption invoked OnOpen")
	}
}

func TestStdAdoptFromItsActualLoopAcceptsPlainAndRejectsLayeredWait(t *testing.T) {
	ev := &Events{Pollers: 1, MaxBufferSize: 64}
	done := startStdTestEvents(t, ev)
	defer stopStdTestEvents(t, ev, done, nil)
	plain := newStdRegistrationConn(53201)
	layered := newStdRegistrationConn(53202)
	outer := &adoptionContractConn{stdRegistrationConn: newStdRegistrationConn(53203)}
	type outcome struct {
		conn Conn
		err  error
	}
	results := make(chan outcome, 2)
	var once atomic.Bool
	outer.onControl = func() {
		if !isEventLoopGoroutine() || !once.CompareAndSwap(false, true) {
			return
		}
		c, err := ev.Adopt(plain)
		results <- outcome{c, err}
		c, err = ev.Adopt(layered, WithLayers(PassThroughLayer))
		results <- outcome{c, err}
	}
	c, err := ev.Adopt(outer)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	first := <-results
	if first.err != nil || first.conn == nil {
		t.Fatalf("same-loop plain adoption=%v", first.err)
	}
	defer first.conn.Close()
	second := <-results
	if second.conn != nil || !errors.Is(second.err, ErrDialOnEventLoop) {
		t.Fatalf("same-loop layered adoption=%v", second.err)
	}
	select {
	case <-layered.closed:
	default:
		t.Fatal("refused same-loop adoption retained transport")
	}
}

func TestStdStoppedRegistrationQueueReturnsOwnershipByClosing(t *testing.T) {
	ev := &Events{Pollers: 1, MaxBufferSize: 64}
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	loop.beginStop(nil)
	loop.runTasks(taskBudget)
	for _, ctx := range []context.Context{nil, context.Background()} {
		raw := newStdRegistrationConn(53301)
		c := &fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: loop}, conn: raw}
		if err := ev.addConnContext(ctx, c); !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
		select {
		case <-raw.closed:
		default:
			t.Fatal("refused queue retained raw connection")
		}
	}
	raw := newStdRegistrationConn(53302)
	if ev.submitAccepted(&fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: loop}, conn: raw}, true) {
		t.Fatal("stopped queue accepted ownership")
	}
	select {
	case <-raw.closed:
	default:
		t.Fatal("refused accept retained raw connection")
	}
	for _, closedEvents := range []bool{false, true} {
		raw := newStdRegistrationConn(53303)
		c := &fdConn{conn: raw}
		if closedEvents {
			ev.closing.Store(true)
			c.loop = loop
		}
		if err := ev.addConnContext(nil, c); !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
		select {
		case <-raw.closed:
		default:
			t.Fatal("unavailable events retained raw connection")
		}
	}
}

func TestStdRegistrationRollbackClearsFDPublication(t *testing.T) {
	ev := &Events{Pollers: 1, MaxBufferSize: 64}
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	for _, fd := range []uintptr{^uintptr(0), uintptr(stdTestFDLimit())} {
		raw := newStdRawRegistrationConn(fd)
		c := &fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: loop}, conn: raw}
		if err := loop.registerConn(c); err == nil {
			t.Fatal("invalid descriptor registered")
		}
		select {
		case <-raw.closed:
		default:
			t.Fatal("failed fd publication retained transport")
		}
	}
	_ = loop.poller.Close(errors.New("registration unavailable"))
	raw := newStdRegistrationConn(53401)
	c := &fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: loop}, conn: raw}
	if err := loop.registerConn(c); err == nil {
		t.Fatal("closed poller accepted registration")
	}
	if loop.fdMap.Get(c.Fd()) != nil {
		t.Fatal("poller failure left a descriptor published")
	}
	select {
	case <-raw.closed:
	default:
		t.Fatal("poller failure retained transport")
	}
}

type stdLifecycleHandler struct{ closed chan error }

func (h stdLifecycleHandler) OnEvent(*poller.Poller, int, poller.Events) {}
func (h stdLifecycleHandler) OnClose(_ *poller.Poller, err error)        { h.closed <- err }

func TestStdLoopClosedPollerReportsCauseAndSealsTasks(t *testing.T) {
	ev := &Events{Pollers: 1, MaxBufferSize: 64}
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("poller failed before serving")
	_ = loop.poller.Close(cause)
	h := stdLifecycleHandler{closed: make(chan error, 1)}
	if err := loop.Serve(true, h); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if err := <-h.closed; !errors.Is(err, cause) {
		t.Fatal(err)
	}
	loop.beginStop(errors.New("later stop"))
	if !loop.stopping.Load() || loop.stopErr != cause || loop.hasPendingTasks() {
		t.Fatal("terminal loop accepted later work")
	}
	// poller.Serve's compatibility lifecycle also delegates final release.
	if err := loop.poller.Serve(false, loop); !errors.Is(err, cause) {
		t.Fatal(err)
	}
}

func TestStdShutdownJoinsLateCloseCallbacks(t *testing.T) {
	loop := new(eventLoop)
	loop.stopIO()
	loop.acquireCloseIO()
	done := make(chan struct{})
	go func() { loop.waitIODrained(); close(done) }()
	select {
	case <-done:
		t.Fatal("late close callback was not joined")
	case <-time.After(5 * time.Millisecond):
	}
	loop.releaseIO()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("released close callback did not unblock shutdown")
	}
	loop.stopIO() // repeating stop after the callback drained is harmless
	if ev := new(Events); ev.selectWorker(7) != nil {
		t.Fatal("uninitialized events selected a worker")
	}
}

func TestStdPublicDialValidationRejectsBeforeOpeningTransport(t *testing.T) {
	if c, err := new(Events).DialContext(context.Background(), "tcp://127.0.0.1:1"); c != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("unstarted dial = %v, %v", c, err)
	}
	ev := &Events{Pollers: 1, MaxBufferSize: 64}
	var opens atomic.Int64
	funcs(ev).Open = func(Conn) { opens.Add(1) }
	done := startStdTestEvents(t, ev)
	defer stopStdTestEvents(t, ev, done, nil)
	ctx := context.Background()
	for _, request := range []struct {
		name string
		dial func() (Conn, error)
	}{
		{"malformed-url-nil-option", func() (Conn, error) { return ev.DialContext(ctx, "tcp://%", nil) }},
		{"call-layer", func() (Conn, error) { return ev.DialContext(ctx, "tcp://127.0.0.1:1", WithLayers(nil)) }},
		{"dialer-layer", func() (Conn, error) { _, err := ev.Dialer(WithLayers(nil)); return nil, err }},
		{"invalid-local-address", func() (Conn, error) { return ev.DialContext(ctx, "tcp://127.0.0.1:1", WithLocalAddr("[invalid")) }},
	} {
		t.Run(request.name, func(t *testing.T) {
			if c, err := request.dial(); c != nil || err == nil {
				t.Fatalf("invalid dial = %v, %v", c, err)
			}
		})
	}
	if opens.Load() != 0 {
		t.Fatal("validation failure opened a connection")
	}
}

func TestStdPublicListenValidationKeepsListenerRegistryUnchanged(t *testing.T) {
	ev := &Events{Pollers: 1, MaxBufferSize: 64}
	done := startStdTestEvents(t, ev)
	defer stopStdTestEvents(t, ev, done, nil)
	if l, err := ev.Listen("tcp://%"); l != nil || err == nil {
		t.Fatalf("malformed listener = %v, %v", l, err)
	}
	cause := errors.New("listener control refused socket")
	var controlled atomic.Bool
	l, err := ev.Listen("tcp://127.0.0.1:0", WithListenControl(func(_, _ string, _ uintptr) error {
		controlled.Store(true)
		return cause
	}))
	if l != nil || !errors.Is(err, cause) || !controlled.Load() {
		t.Fatalf("refused listener = %v, %v, control=%v", l, err, controlled.Load())
	}
	ev.acceptor.mux.Lock()
	listeners := len(ev.acceptor.listeners)
	ev.acceptor.mux.Unlock()
	if listeners != 0 {
		t.Fatal("failed listener remained published")
	}
}

type stdCancellationOnClose struct {
	*stdRegistrationConn
	cancel func()
}

func (c stdCancellationOnClose) Close() error {
	err := c.stdRegistrationConn.Close()
	c.cancel()
	return err
}

func TestStdQueueRefusalKeepsConcurrentCancellationCause(t *testing.T) {
	ev := &Events{Pollers: 1, MaxBufferSize: 64}
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	loop.beginStop(nil)
	loop.runTasks(taskBudget)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("cancelled while transferring ownership")
	raw := stdCancellationOnClose{newStdRegistrationConn(53501), func() { cancel(cause) }}
	c := &fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: loop}, conn: raw}
	if err := ev.addConnContext(ctx, c); !errors.Is(err, cause) {
		t.Fatalf("concurrent registration cancellation = %v", err)
	}
	select {
	case <-raw.closed:
	default:
		t.Fatal("refused queue retained the cancelled transport")
	}
}
