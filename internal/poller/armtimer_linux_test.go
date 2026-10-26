//go:build linux && !stdio

package poller

import (
	"sync"
	"testing"
	"time"
)

// Arming races a concurrent Close: the descriptor check and the system call
// both run under ctl, so the release closing the timerfd cannot hand its
// number to another file under them. What -race would catch here without the
// lock is the unlocked first read of timerfd against release's write.
func TestArmTimerRacesClose(t *testing.T) {
	for range 100 {
		poller, err := New()
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = poller.ArmTimer(time.Millisecond)
				_ = poller.ArmTimer(0)
			}
		}()
		go func() {
			defer wg.Done()
			_ = poller.Close(nil)
		}()
		wg.Wait()
	}
}
