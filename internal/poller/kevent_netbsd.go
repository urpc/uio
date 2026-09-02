//go:build netbsd && !stdio

package poller

import "golang.org/x/sys/unix"

func makeKevent(fd int, filter, flags int64) unix.Kevent_t {
	return unix.Kevent_t{
		Ident:  uint64(fd),
		Filter: uint32(filter),
		Flags:  uint32(flags),
	}
}
