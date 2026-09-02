//go:build (darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package poller

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBSDWakeTreatsFullPipeAsCoalesced(t *testing.T) {
	poller, err := NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	defer poller.Close(nil)
	if err = poller.deleteFilter(-1, readEvents); err != nil {
		t.Fatalf("delete absent read filter = %v", err)
	}

	data := make([]byte, 4096)
	for {
		_, err = unix.Write(poller.wakeWrite, data)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = poller.Wake(); err != nil {
		t.Fatalf("Wake with a full pipe = %v", err)
	}
}
