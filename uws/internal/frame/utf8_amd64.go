//go:build amd64 && !purego

package frame

import (
	"unicode/utf8"

	segutf8 "github.com/segmentio/asm/utf8"
)

// vectorUTF8Min is where the AVX2 validator starts; below it, it would only
// scan for ASCII before calling utf8.Valid anyway.
const vectorUTF8Min = 32

// ValidUTF8 reports whether p is valid UTF-8, exactly as utf8.Valid does, with
// the AVX2 validator from vectorUTF8Min bytes where the CPU has it.
func ValidUTF8(p []byte) bool {
	if useAVX2 && len(p) >= vectorUTF8Min {
		return segutf8.Valid(p)
	}
	return utf8.Valid(p)
}
