//go:build amd64 && !purego

package frame

import "golang.org/x/sys/cpu"

var useAVX2 = cpu.X86.HasAVX2

// maskAVX2 XORs n bytes at b, n a multiple of 32, with key repeated.
//
//go:noescape
func maskAVX2(b *byte, n int, key uint64)

// maskASCIIAVX2 is maskAVX2 that also returns the high bits of every result
// byte ORed together, zero when the unmasked bytes are all ASCII.
//
//go:noescape
func maskASCIIAVX2(b *byte, n int, key uint64) uint32
