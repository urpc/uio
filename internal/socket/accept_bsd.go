//go:build darwin || netbsd || freebsd || openbsd || dragonfly

package socket

import "syscall"

// Accept protects the non-atomic close-on-exec setup from a concurrent fork.
func Accept(fd int) (nfd int, addr syscall.Sockaddr, err error) {
	syscall.ForkLock.RLock()
	nfd, addr, err = syscall.Accept(fd)
	if err == nil {
		syscall.CloseOnExec(nfd)
		if err = syscall.SetNonblock(nfd, true); err != nil {
			_ = syscall.Close(nfd)
		}
	}
	syscall.ForkLock.RUnlock()
	return nfd, addr, err
}
