//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package poller

import (
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Exercise actual descriptor exhaustion rather than swapping syscall hooks.
// The descriptor limit and opened files are restored before reporting any
// failure; package tests do not run in parallel during this critical section.
func TestPollerConstructionReleasesPartiallyAcquiredDescriptors(t *testing.T) {
	existing, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close(nil)
	var original unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
		t.Fatal(err)
	}
	limit := original
	if limit.Cur > 64 {
		limit.Cur = 64
	}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	var opened []int
	defer func() {
		_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &original)
		for _, fd := range opened {
			_ = unix.Close(fd)
		}
	}()
	fill := func() (int, error) {
		n := 0
		for {
			fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.EMFILE) {
				return n, nil
			}
			if err != nil {
				return n, err
			}
			opened = append(opened, fd)
			n++
		}
	}
	_, failure := fill()
	if failure == nil && len(opened) < 3 {
		failure = fmt.Errorf("insufficient free descriptors for resource test")
	}
	for slots := 0; slots <= 2 && failure == nil; slots++ {
		for range slots {
			last := len(opened) - 1
			_ = unix.Close(opened[last])
			opened = opened[:last]
		}
		p, err := New()
		if runtime.GOOS == "linux" && slots == 2 {
			// epoll + eventfd still operate when timerfd allocation fails.
			if err != nil || p == nil {
				failure = fmt.Errorf("timerless construction: %v", err)
			} else if err := p.ArmTimer(time.Millisecond); !errors.Is(err, ErrTimers) {
				failure = fmt.Errorf("timerless arm = %v", err)
			}
		} else if !errors.Is(err, unix.EMFILE) || p != nil {
			failure = fmt.Errorf("construction with %d free slots = %v, %v", slots, p, err)
		}
		if p != nil {
			_ = p.Close(nil)
		}
		reclaimed, err := fill()
		if err != nil || reclaimed != slots {
			failure = fmt.Errorf("construction leaked descriptors: reclaimed=%d want=%d error=%v", reclaimed, slots, err)
		}
	}
	if runtime.GOOS == "linux" && failure == nil {
		if _, err := existing.WaitBatch(new(Batch), nil, 0); !errors.Is(err, unix.EMFILE) {
			failure = fmt.Errorf("waiter with exhausted descriptor budget = %v", err)
		}
	}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
		t.Fatal(err)
	}
	for _, fd := range opened {
		_ = unix.Close(fd)
	}
	opened = nil
	if failure != nil {
		t.Fatal(failure)
	}
}
