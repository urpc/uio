//go:build !unix

package uio

import "time"

func processCPUTime() time.Duration { return 0 }
