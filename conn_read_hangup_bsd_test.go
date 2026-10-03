//go:build (darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import "golang.org/x/sys/unix"

// readHangupQueued reports whether fd's peer has closed or reset its side.
func readHangupQueued(fd int) (bool, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return false, err
	}
	defer unix.Close(kq)
	changes := make([]unix.Kevent_t, 1)
	unix.SetKevent(&changes[0], fd, unix.EVFILT_READ, unix.EV_ADD)
	events := make([]unix.Kevent_t, 1)
	n, err := unix.Kevent(kq, changes, events, &unix.Timespec{})
	if err != nil {
		return false, err
	}
	return n == 1 && events[0].Flags&unix.EV_EOF != 0, nil
}
