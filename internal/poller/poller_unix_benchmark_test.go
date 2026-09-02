//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package poller

import "testing"

func BenchmarkWatchUnchangedInterest(b *testing.B) {
	poller, err := NewNetPoller()
	if err != nil {
		b.Fatal(err)
	}
	defer poller.Close(nil)
	poller.interests[1] = Readable
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err = poller.Watch(1, Readable); err != nil {
			b.Fatal(err)
		}
	}
}
