package uio

import "github.com/petermattis/goid"

// currentGoroutineID is isolated here so inLoop can be replaced without
// exposing the runtime-dependent representation to the rest of the package.
func currentGoroutineID() int64 {
	return goid.Get()
}
