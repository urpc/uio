package uio

import (
	"runtime"
	"testing"
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
