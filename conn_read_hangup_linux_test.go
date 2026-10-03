//go:build linux && !stdio

package uio

import "golang.org/x/sys/unix"

// readHangupQueued reports whether fd's peer has closed or reset its side.
func readHangupQueued(fd int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLRDHUP}}
	n, err := unix.Poll(fds, 0)
	if err != nil {
		return false, err
	}
	return n == 1 && fds[0].Revents&(unix.POLLRDHUP|unix.POLLHUP|unix.POLLERR) != 0, nil
}
