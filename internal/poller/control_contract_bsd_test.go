//go:build darwin || netbsd || freebsd || openbsd || dragonfly

package poller

import (
	"errors"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBSDTimerArmingDisarmingAndExpiry(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(nil)
	if err := p.ArmTimer(0); err != nil {
		t.Fatal(err)
	}
	var ticks atomic.Int64
	p.SetTimerSink(func() { ticks.Add(1) })
	if err := p.ArmTimer(time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Wait(nil, 1000); n != 0 || err != nil || ticks.Load() != 1 {
		t.Fatalf("timer expiry=%d,%v ticks=%d", n, err, ticks.Load())
	}
	if err := p.ArmTimer(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := p.ArmTimer(0); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Wait(nil, 5); n != 0 || err != nil || ticks.Load() != 1 {
		t.Fatal("disarmed timer fired", n, err)
	}
	p.SetTimerSink(nil)
	if err := p.ArmTimer(time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(nil, 1000); err != nil {
		t.Fatal(err)
	}
	_ = p.Close(nil)
	if err := p.ArmTimer(time.Millisecond); err != nil {
		t.Fatalf("arming closed timer = %v", err)
	}
	p.drainWake() // a closed wake pipe must not be drained or re-used
	if n, err := p.WaitBatchParked(new(Batch), nil); n != 0 || err != nil {
		t.Fatalf("closed parked wait = %d, %v", n, err)
	}
}

func TestBSDParkedWaitFallsBackAfterRuntimeDescriptorClosure(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(nil)
	// Some BSDs cannot make a kqueue duplicate nonblocking. A real pollable
	// pipe supplies the same runtime RawConn boundary, which then fails on
	// Close before its callback runs, exercising the fallback without hooks.
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	if err := unix.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fds[0]), "failed-runtime-park")
	defer file.Close()
	conn, err := file.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	batch := Batch{parkTried: true, parkFile: file, parkConn: conn, parkKq: p.kqfd}
	batch.parkRead = batch.readParked
	if err := batch.parkFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Wake(); err != nil {
		t.Fatal(err)
	}
	if n, err := p.WaitBatchParked(&batch, nil); n != 0 || err != nil {
		t.Fatalf("fallback wait=%d,%v", n, err)
	}
	if batch.parkFile != nil || batch.parkConn != nil {
		t.Fatal("failed runtime descriptor was retained")
	}
	p.dropPark(&batch) // dropping an already-failed park is harmless
}

func TestBSDBrokenKernelDescriptorSurfacesErrors(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	// Model the kernel descriptor becoming unusable. Clear its owned number
	// after closing so cleanup cannot close a subsequently reused descriptor.
	if err := unix.Close(p.kqfd); err != nil {
		t.Fatal(err)
	}
	p.kqfd = -1
	defer p.Close(nil)
	if _, err := p.Wait(nil, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("broken wait=%v", err)
	}
	if err := p.ArmTimer(time.Millisecond); !errors.Is(err, unix.EBADF) {
		t.Fatalf("broken timer=%v", err)
	}
	var batch Batch
	p.mountPark(&batch)
	if !batch.parkTried || batch.parkFile != nil {
		t.Fatal("broken descriptor installed runtime park")
	}
}

func TestBSDInvalidRegistrationAndCloseSerialize(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(nil)
	for _, want := range []Interest{Readable, Writable} {
		if err := p.Add(-1, want); !errors.Is(err, unix.EBADF) {
			t.Fatalf("invalid add=%v", err)
		}
	}
	p.ctl.Lock()
	added := make(chan error, 1)
	started := make(chan struct{})
	go func() { close(started); added <- p.Add(-1, Readable) }()
	<-started
	// Give the in-flight Add its turn to block behind the control barrier.
	runtime.Gosched()
	time.Sleep(time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- p.Close(nil) }()
	deadline := time.Now().Add(time.Second)
	for !p.Closed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	p.ctl.Unlock()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-added; !errors.Is(err, unix.EBADF) {
		t.Fatalf("add racing close=%v", err)
	}
}

func TestBSDCloseKeepsWakeReadableWhileAnotherWaiterDrains(t *testing.T) {
	for range 32 {
		p, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := p.beginWait(); !ok || err != nil {
			t.Fatal(err)
		}
		var writes atomic.Int64
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			data := make([]byte, 4096)
			for !p.Closed() {
				if n, _ := unix.Write(p.wakeWrite, data); n > 0 {
					writes.Add(1)
				}
				runtime.Gosched()
			}
		}()
		for writes.Load() == 0 {
			runtime.Gosched()
		}
		drained := make(chan struct{})
		go func() { p.drainWake(); close(drained) }()
		time.Sleep(100 * time.Microsecond)
		if err := p.Close(nil); err != nil {
			t.Fatal(err)
		}
		<-writerDone
		<-drained
		var byteRead [1]byte
		if n, err := unix.Read(p.wakeRead, byteRead[:]); n != 1 || err != nil {
			t.Fatalf("close wake disappeared after drain: %d, %v", n, err)
		}
		p.finishWait()
	}
}
