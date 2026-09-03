package uio

import "testing"

func TestServeInitializesAtMostOneListener(t *testing.T) {
	for _, test := range []struct {
		name string
		addr string
		want int
	}{
		{name: "dial only", want: 0},
		{name: "listener", addr: "tcp://127.0.0.1:0", want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &Events{Pollers: 1}
			events.OnStart = func(events *Events) {
				if got := len(events.acceptor.listeners); got != test.want {
					t.Errorf("listeners = %d, want %d", got, test.want)
				}
				_ = events.Close(nil)
			}
			var err error
			if test.addr == "" {
				err = events.Serve()
			} else {
				err = events.Serve(test.addr)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServeRejectsMultipleAddressesBeforeInitialization(t *testing.T) {
	events := &Events{}
	err := events.Serve("tcp://127.0.0.1:0", "tcp://127.0.0.1:0")
	if err != ErrTooManyListenAddresses {
		t.Fatalf("Serve error = %v, want %v", err, ErrTooManyListenAddresses)
	}
	if events.master != nil || events.acceptor != nil || len(events.workers) != 0 {
		t.Fatal("rejected Serve initialized event-loop resources")
	}
}
