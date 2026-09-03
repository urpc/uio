//go:build (darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package poller

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestBSDCallerOwnedInterestTransitions(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	poller, err := NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	defer poller.Close(nil)
	if err = poller.Add(fds[0], Readable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Modify(fds[0], Readable, Readable|Writable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Modify(fds[0], Readable|Writable, Readable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Remove(fds[0], Readable); err != nil {
		t.Fatal(err)
	}
}
