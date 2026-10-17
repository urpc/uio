//go:build !amd64 || purego

package frame

const useAVX2 = false

func maskAVX2(*byte, int, uint64) {}

func maskASCIIAVX2(*byte, int, uint64) uint32 { return 0 }
