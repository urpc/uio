package frame

import "encoding/binary"

const wordUnmaskThreshold = 16

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
