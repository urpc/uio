package frame

import "encoding/binary"

const (
	wordUnmaskThreshold = 16
	// Below this the vector setup costs more than the word loop it replaces.
	vectorUnmaskThreshold = 128
)

// VectorUnmaskMin is the payload length from which Unmask takes the vector
// path where the CPU has one.
const VectorUnmaskMin = vectorUnmaskThreshold

// Unmask removes a frame's mask from its whole payload in place.
func Unmask(payload []byte, key [4]byte) { unmask(payload, key, 0) }

// UnmaskASCII is Unmask that also reports whether every unmasked byte is
// ASCII, which settles a text payload's UTF-8 check in the same pass. Like
// Unmask it takes the vector path from VectorUnmaskMin bytes where the CPU
// has one, and folds the high bits in as it goes.
func UnmaskASCII(payload []byte, key [4]byte) bool {
	if len(payload) < wordUnmaskThreshold {
		var seen byte
		for i := range payload {
			b := payload[i] ^ key[i&3]
			payload[i] = b
			seen |= b
		}
		return seen < 0x80
	}

	key32 := binary.LittleEndian.Uint32(key[:])
	key64 := uint64(key32) | uint64(key32)<<32
	var seen uint64
	if useAVX2 && len(payload) >= vectorUnmaskThreshold {
		// A multiple of 32 keeps the key phase for the tail below.
		n := len(payload) &^ 31
		if maskASCIIAVX2(&payload[0], n, key64) != 0 {
			seen = 0x80
		}
		payload = payload[n:]
	}
	for len(payload) >= 32 {
		w0 := binary.LittleEndian.Uint64(payload) ^ key64
		w1 := binary.LittleEndian.Uint64(payload[8:]) ^ key64
		w2 := binary.LittleEndian.Uint64(payload[16:]) ^ key64
		w3 := binary.LittleEndian.Uint64(payload[24:]) ^ key64
		binary.LittleEndian.PutUint64(payload, w0)
		binary.LittleEndian.PutUint64(payload[8:], w1)
		binary.LittleEndian.PutUint64(payload[16:], w2)
		binary.LittleEndian.PutUint64(payload[24:], w3)
		seen |= w0 | w1 | w2 | w3
		payload = payload[32:]
	}
	for len(payload) >= 8 {
		w := binary.LittleEndian.Uint64(payload) ^ key64
		binary.LittleEndian.PutUint64(payload, w)
		seen |= w
		payload = payload[8:]
	}
	// Whole words came before, so the key phase is back at its start.
	for i := range payload {
		b := payload[i] ^ key[i&3]
		payload[i] = b
		seen |= uint64(b)
	}
	return seen&0x8080808080808080 == 0
}

// unmask applies the repeating four-byte mask starting at the frame-relative
// offset. The word-at-a-time body handles native endian order explicitly while
// prefix and suffix loops cover unaligned bytes. On amd64 with AVX2 the bulk
// of a larger payload is masked 32 bytes at a time.
func unmask(payload []byte, key [4]byte, offset int) {
	offset &= 3
	if len(payload) < wordUnmaskThreshold {
		for i := range payload {
			payload[i] ^= key[(offset+i)&3]
		}
		return
	}

	key32 := uint32(key[offset]) |
		uint32(key[(offset+1)&3])<<8 |
		uint32(key[(offset+2)&3])<<16 |
		uint32(key[(offset+3)&3])<<24
	key64 := uint64(key32) | uint64(key32)<<32

	if useAVX2 && len(payload) >= vectorUnmaskThreshold {
		// A multiple of 32 keeps the key phase for the tail below.
		n := len(payload) &^ 31
		maskAVX2(&payload[0], n, key64)
		payload = payload[n:]
	}
	for len(payload) >= 32 {
		binary.LittleEndian.PutUint64(payload, binary.LittleEndian.Uint64(payload)^key64)
		binary.LittleEndian.PutUint64(payload[8:], binary.LittleEndian.Uint64(payload[8:])^key64)
		binary.LittleEndian.PutUint64(payload[16:], binary.LittleEndian.Uint64(payload[16:])^key64)
		binary.LittleEndian.PutUint64(payload[24:], binary.LittleEndian.Uint64(payload[24:])^key64)
		payload = payload[32:]
	}
	for len(payload) >= 8 {
		binary.LittleEndian.PutUint64(payload, binary.LittleEndian.Uint64(payload)^key64)
		payload = payload[8:]
	}
	for i := range payload {
		payload[i] ^= key[(offset+i)&3]
	}
}
