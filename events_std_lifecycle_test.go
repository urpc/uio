//go:build stdio && !windows

package uio

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

type adoptionContractConn struct {
	*stdRegistrationConn
	local net.Addr
}

func (c *adoptionContractConn) LocalAddr() net.Addr {
	if c.local != nil {
		return c.local
	}
	return c.stdRegistrationConn.LocalAddr()
}

func TestStdAdoptFailureAlwaysReleasesTransferredOwnership(t *testing.T) {
	ev := &Events{Pollers: 1, ReadBufferSize: 64}
	var opens atomic.Int64
	funcs(ev).Open = func(Conn) {
		opens.Add(1)
	}
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
	}{{"nil-layer", &adoptionContractConn{stdRegistrationConn: newStdRegistrationConn(53101)}, []AdoptOption{WithLayers(nil)}, context.Background(), nil}, {"packet-layer", &adoptionContractConn{stdRegistrationConn: newStdRegistrationConn(53102), local: &net.UDPAddr{}}, []AdoptOption{WithLayers(PassThroughLayer)}, context.Background(), ErrLayersNeedStream}, {"cancelled-registration", &adoptionContractConn{stdRegistrationConn: newStdRegistrationConn(53105)}, nil, ctx, failure}}
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
	if opens.Load() != 0 {
		t.Fatal("failed adoption invoked OnOpen")
	}
}

func TestStdAdoptSupportsNetPipeWithoutDescriptor(t *testing.T) {
	ev := &Events{Pollers: 1, ReadBufferSize: 64}
	funcs(ev).Read = func(c Conn) error {
		_, err := c.WriteTo(c)
		return err
	}
	done := startStdTestEvents(t, ev)
	defer stopStdTestEvents(t, ev, done, nil)
	first, second := net.Pipe()
	defer second.Close()
	adopted, err := ev.Adopt(first)
	if err != nil {
		t.Fatalf("Adopt(net.Pipe) = %v", err)
	}
	defer adopted.Close()
	if sc, ok := SocketOf(adopted); ok {
		if err := sc.Control(func(uintptr) {
		}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("Control on descriptorless pipe = %v, want ErrUnsupported", err)
		}
	}
	if _, err := second.Write([]byte("pipe-ok")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 7)
	if _, err := io.ReadFull(second, got); err != nil || string(got) != "pipe-ok" {
		t.Fatalf("read = %q, %v", got, err)
	}
}

func TestStdAdoptFromItsActualLoopAcceptsPlainAndRejectsLayeredWait(t *testing.T) {
	ev := &Events{Pollers: 1, ReadBufferSize: 64}
	done := startStdTestEvents(t, ev)
	defer stopStdTestEvents(t, ev, done, nil)
	plain := newStdRegistrationConn(53201)
	layered := newStdRegistrationConn(53202)
	outer := newStdRegistrationConn(53203)
	type outcome struct {
		conn Conn
		err  error
	}
	results := make(chan outcome, 2)
	var once atomic.Bool
	registeredForTest = func(*fdConn) {
		if !isEventLoopGoroutine() || !once.CompareAndSwap(false, true) {
			return
		}
		c, err := ev.Adopt(plain)
		results <- outcome{c, err}
		c, err = ev.Adopt(layered, WithLayers(PassThroughLayer))
		results <- outcome{c, err}
	}
	t.Cleanup(func() {
		registeredForTest = nil
	})
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
	ev := &Events{Pollers: 1, ReadBufferSize: 64}
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.closeLoop(nil)
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
	if ev.submitAccepted(&fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: loop}, conn: raw}) {
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

func TestStdClosedLoopReportsCauseAndRejectsRegistration(t *testing.T) {
	ev := &Events{Pollers: 1, ReadBufferSize: 64}
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("loop closed before serving")
	loop.closeLoop(cause)
	raw := newStdRegistrationConn(53401)
	c := &fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: loop}, conn: raw}
	if err := loop.registerConn(c); !errors.Is(err, cause) {
		t.Fatalf("closed loop registerConn = %v, want %v", err, cause)
	}
	select {
	case <-raw.closed:
	default:
		t.Fatal("closed loop retained transport")
	}
	var closeHookErr error
	if err := loop.Serve(true, func(err error) {
		closeHookErr = err
	}); !errors.Is(err, cause) {
		t.Fatalf("Serve = %v, want %v", err, cause)
	}
	if !errors.Is(closeHookErr, cause) {
		t.Fatalf("onClose = %v, want %v", closeHookErr, cause)
	}
	loop.beginStop(errors.New("later stop"))
	if !loop.stopping.Load() || loop.stopErr != cause || loop.hasPendingTasks() {
		t.Fatal("terminal loop accepted later work")
	}
	if ev := new(Events); ev.selectWorker() != nil {
		t.Fatal("uninitialized events selected a worker")
	}
}

func TestStdPublicDialValidationRejectsBeforeOpeningTransport(t *testing.T) {
	if c, err := new(Events).DialContext(context.Background(), "tcp://127.0.0.1:1"); c != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("unstarted dial = %v, %v", c, err)
	}
	ev := &Events{Pollers: 1, ReadBufferSize: 64}
	var opens atomic.Int64
	funcs(ev).Open = func(Conn) {
		opens.Add(1)
	}
	done := startStdTestEvents(t, ev)
	defer stopStdTestEvents(t, ev, done, nil)
	ctx := context.Background()
	for _, request := range []struct {
		name string
		dial func() (Conn, error)
	}{{"malformed-url-nil-option", func() (Conn, error) {
		return ev.DialContext(ctx, "tcp://%", nil)
	}}, {"call-layer", func() (Conn, error) {
		return ev.DialContext(ctx, "tcp://127.0.0.1:1", WithLayers(nil))
	}}, {"dialer-layer", func() (Conn, error) {
		_, err := ev.Dialer(WithLayers(nil))
		return nil, err
	}}, {"invalid-local-address", func() (Conn, error) {
		return ev.DialContext(ctx, "tcp://127.0.0.1:1", WithLocalAddr("[invalid"))
	}}} {
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
	ev := &Events{Pollers: 1, ReadBufferSize: 64}
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
	ev := &Events{Pollers: 1, ReadBufferSize: 64}
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.closeLoop(nil)
	loop.beginStop(nil)
	loop.runTasks(taskBudget)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("cancelled while transferring ownership")
	raw := stdCancellationOnClose{newStdRegistrationConn(53501), func() {
		cancel(cause)
	}}
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
