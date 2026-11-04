//go:build unix

package uio

import (
	"syscall"
	"time"
)

// processCPUTime is the user plus system CPU time the process has used.
func processCPUTime() time.Duration {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
