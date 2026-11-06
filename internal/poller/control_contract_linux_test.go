//go:build linux

package poller

import (
	"encoding/binary"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLinuxTimerAndPrivateWakeAreDistinctControlEvents(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(nil)
	var ticks atomic.Int64
	p.SetTimerSink(func() { ticks.Add(1) })
	if err := p.ArmTimer(time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Wait(nil, 1000); n != 0 || err != nil || ticks.Load() != 1 {
		t.Fatalf("timer=%d,%v ticks=%d", n, err, ticks.Load())
	}
	p.drainTimer() // another waiter can observe an already-retired timer edge
	if ticks.Load() != 1 {
		t.Fatal("empty timer edge duplicated expiry")
	}
	if err := p.ArmTimer(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := p.ArmTimer(0); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(nil, 2); err != nil || ticks.Load() != 1 {
		t.Fatalf("disarm=%v ticks=%d", err, ticks.Load())
	}
	p.SetTimerSink(nil)
	if err := p.ArmTimer(time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(nil, 1000); err != nil {
		t.Fatal(err)
	}
	var batch Batch
	if ok, err := p.beginWait(&batch); !ok || err != nil {
		t.Fatal(err)
	}
	p.finishWait()
	if err := raiseWake(batch.wakefd); err != nil {
		t.Fatal(err)
	}
	if n, err := p.WaitBatch(&batch, nil, 0); n != 0 || err != nil {
		t.Fatalf("private wake=%d,%v", n, err)
	}
}

func TestLinuxParkFailureFallsBackAndInvalidKernelFDDoesNotLeakWaiter(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(nil)
	var batch Batch
	p.mountPark(&batch)
	if batch.parkFile == nil {
		t.Fatal("epoll runtime park was unavailable")
	}
	if err := batch.parkFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Wake(); err != nil {
		t.Fatal(err)
	}
	if n, err := p.WaitBatchParked(&batch, nil); n != 0 || err != nil {
		t.Fatalf("fallback=%d,%v", n, err)
	}
	if batch.parkFile != nil || batch.parkConn != nil {
		t.Fatal("failed park descriptor retained")
	}
	if err := unix.Close(p.epfd); err != nil {
		t.Fatal(err)
	}
	p.epfd = -1 // cleanup must not close a descriptor whose number can be reused
	if _, err := p.WaitBatch(new(Batch), nil, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("new waiter on broken poller=%v", err)
	}
	if _, err := p.WaitBatch(&batch, nil, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("existing waiter on broken poller=%v", err)
	}
	var failed Batch
	p.mountPark(&failed)
	if !failed.parkTried || failed.parkFile != nil {
		t.Fatal("broken epoll installed a park")
	}
	previous := rawProbeEnabled
	defer func() { rawProbeEnabled = previous }()
	for _, raw := range []bool{false, true} {
		rawProbeEnabled = raw
		failed.parkEpfd = -1
		if !failed.readParked(0) || !errors.Is(failed.parkErr, unix.EBADF) {
			t.Fatalf("raw=%v invalid probe=%v", raw, failed.parkErr)
		}
	}
	if p.waiters != 0 {
		t.Fatal("failed waiter remained registered")
	}
}

func TestLinuxRegistrationValidationSerializesWithClose(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(nil)
	if err := p.Register(-1, 0, true, 7); !errors.Is(err, errInvalidInterest) {
		t.Fatal(err)
	}
	p.ctl.Lock()
	results := make(chan error, 2)
	started := make(chan struct{}, 2)
	go func() { started <- struct{}{}; results <- p.Register(-1, Readable, true, 7) }()
	go func() { started <- struct{}{}; results <- p.Add(-1, Readable) }()
	<-started
	<-started
	runtime.Gosched()
	time.Sleep(time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- p.Close(nil) }()
	for !p.Closed() {
		runtime.Gosched()
	}
	p.ctl.Unlock()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-results; !errors.Is(err, unix.EBADF) {
			t.Fatal(err)
		}
	}
	if err := p.Register(-1, Readable, false, 0); !errors.Is(err, unix.EBADF) {
		t.Fatal(err)
	}
}

func TestLinuxSaturatedWakeCounterIsAlreadyReady(t *testing.T) {
	fd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var value [8]byte
	binary.NativeEndian.PutUint64(value[:], ^uint64(0)-1)
	if _, err := unix.Write(fd, value[:]); err != nil {
		t.Fatal(err)
	}
	if err := raiseWake(fd); err != nil {
		t.Fatal(err)
	}
	if n, err := unix.Read(fd, value[:]); n != 8 || err != nil || binary.NativeEndian.Uint64(value[:]) != ^uint64(0)-1 {
		t.Fatalf("saturated wake=%d,%v", n, err)
	}
}
