package uio

import (
	"context"
	"errors"
	"net"
	"runtime"
	"testing"
	"time"
)

func TestPollerCountDefaultsAndLimits(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured int
		want       int
	}{
		{name: "zero", want: min(4, runtime.NumCPU())},
		{name: "negative", configured: -1, want: min(4, runtime.NumCPU())},
		{name: "explicit", configured: 1, want: 1},
		{name: "above CPU count", configured: runtime.NumCPU() + 1, want: runtime.NumCPU()},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &Events{Pollers: test.configured}
			if err := events.initConfig(); err != nil {
				t.Fatal(err)
			}
			if events.Pollers != test.want {
				t.Fatalf("Pollers = %d, want %d", events.Pollers, test.want)
			}
		})
	}
}

func TestDialRejectsAnotherEventsLoop(t *testing.T) {
	owner := currentGoroutineID()
	activeEventLoops.Store(owner, struct{}{})
	defer activeEventLoops.Delete(owner)
	events := &Events{}
	events.ready.Store(true)
	if _, err := events.DialContext(context.Background(), "tcp://127.0.0.1:1", nil); !errors.Is(err, ErrDialOnEventLoop) {
		t.Fatalf("DialContext from another Events loop = %v, want ErrDialOnEventLoop", err)
	}
}

func TestEventLoopShutdownClosesIOAdmission(t *testing.T) {
	loop := &eventLoop{ioIdle: make(chan struct{})}
	if !loop.acquireIO() {
		t.Fatal("loop rejected I/O before shutdown")
	}
	released := false
	defer func() {
		if !released {
			loop.releaseIO()
		}
	}()
	stopped := make(chan struct{})
	go func() {
		loop.stopIO()
		close(stopped)
	}()
	deadline := time.Now().Add(time.Second)
	for !loop.ioStopped() {
		if time.Now().After(deadline) {
			t.Fatal("loop did not close I/O admission")
		}
		runtime.Gosched()
	}
	if loop.acquireIO() {
		loop.releaseIO()
		t.Fatal("loop accepted I/O after stop")
	}
	select {
	case <-stopped:
		t.Fatal("shutdown returned before active I/O completed")
	default:
	}
	loop.releaseIO()
	released = true
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not complete after I/O returned")
	}
	loop.acquireCloseIO()
	loop.releaseIO()
}

func TestReadBufferPoolDoesNotAllocateOnReuse(t *testing.T) {
	events := &Events{MaxBufferSize: 16 * 1024}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	holder := events.readPool.Get().(*readBuffer)
	events.readPool.Put(holder)
	allocations := testing.AllocsPerRun(1000, func() {
		holder = events.readPool.Get().(*readBuffer)
		events.readPool.Put(holder)
	})
	if allocations != 0 {
		t.Fatalf("read buffer pool allocations = %.2f, want 0", allocations)
	}
}

func TestEventLifecycleHelpers(t *testing.T) {
	events := &Events{}
	request := &registerRequest{ctx: context.Background()}
	if err := request.cause(); !errors.Is(err, context.Canceled) {
		t.Fatalf("uncanceled request cause = %v, want context.Canceled", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- events.Wait() }()
	select {
	case <-waitDone:
		t.Fatal("Wait returned before Close")
	case <-time.After(20 * time.Millisecond):
	}
	shutdownErr := errors.New("shutdown")
	if err := events.Close(shutdownErr); err != nil {
		t.Fatal(err)
	}
	if err := <-waitDone; !errors.Is(err, shutdownErr) {
		t.Fatalf("Wait error = %v, want %v", err, shutdownErr)
	}
	if err := events.Serve(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve after close = %v, want net.ErrClosed", err)
	}
}

func TestWaitIncludesOnStop(t *testing.T) {
	events := &Events{Pollers: 1}
	started := make(chan struct{})
	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})
	events.OnStart = func(*Events) { close(started) }
	events.OnStop = func(*Events) {
		close(stopStarted)
		<-releaseStop
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	<-started
	shutdownErr := errors.New("wait for on-stop")
	if err := events.Close(shutdownErr); err != nil {
		t.Fatal(err)
	}
	<-stopStarted
	waitDone := make(chan error, 1)
	go func() { waitDone <- events.Wait() }()
	select {
	case <-waitDone:
		t.Fatal("Wait returned before OnStop")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseStop)
	if err := <-waitDone; !errors.Is(err, shutdownErr) {
		t.Fatalf("Wait error = %v, want %v", err, shutdownErr)
	}
	if err := <-serveDone; !errors.Is(err, shutdownErr) {
		t.Fatalf("Serve error = %v, want %v", err, shutdownErr)
	}
}
