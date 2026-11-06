package writecontrol

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestControlNilAndInexactLifecycle(t *testing.T) {
	var nilCtrl *Control
	if nilCtrl.Inexact() {
		t.Fatal("nil control reported inexact")
	}
	nilCtrl.Watch(func(int) { t.Fatal("nil control ran watcher") })
	nilCtrl.Notify(10)

	exact := New(false)
	if exact.Inexact() {
		t.Fatal("exact control reported inexact")
	}
	exact.Notify(4)
	exact.Watch(nil)

	inexact := New(true)
	if !inexact.Inexact() {
		t.Fatal("inexact control did not report inexact")
	}
}

func TestControlConcurrentWatchAndNotify(t *testing.T) {
	ctrl := New(false)
	var hits atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctrl.Watch(func(n int) { hits.Add(int64(n)) })
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				ctrl.Notify(1)
			}
		}()
	}
	wg.Wait()
	hits.Store(0)
	ctrl.Notify(1)
	if got := hits.Load(); got != 8 {
		t.Fatalf("watchers invoked = %d, want 8", got)
	}
}
