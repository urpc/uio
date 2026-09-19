package uio

import (
	"context"
	"errors"
	"net"
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

func TestEventLifecycleHelpers(t *testing.T) {
	events := &Events{}
	events.callbackWG.Add(1)
	callbackID := events.enterExternalCallback()
	if _, exists := events.callbackGoids.Load(callbackID); !exists {
		t.Fatal("external callback was not registered")
	}
	events.finishExternalCallback(callbackID)
	events.callbackWG.Wait()
	if _, exists := events.callbackGoids.Load(callbackID); exists {
		t.Fatal("finished external callback remained registered")
	}

	request := &registerRequest{ctx: context.Background()}
	if err := request.cause(); !errors.Is(err, context.Canceled) {
		t.Fatalf("uncanceled request cause = %v, want context.Canceled", err)
	}

	events.closing.Store(true)
	if err := events.Serve(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve after close = %v, want net.ErrClosed", err)
	}
}
