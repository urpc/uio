//go:build amd64 && !purego

package frame

import "golang.org/x/sys/cpu"

var useAVX2 = cpu.X86.HasAVX2

// maskAVX2 XORs n bytes at b, n a multiple of 32, with key repeated.
//
//go:noescape
func maskAVX2(b *byte, n int, key uint64)
