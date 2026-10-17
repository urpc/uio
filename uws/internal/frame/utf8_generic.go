//go:build !amd64 || purego

package frame

import "unicode/utf8"

const vectorUTF8Min = 32

// ValidUTF8 reports whether p is valid UTF-8, exactly as utf8.Valid does.
func ValidUTF8(p []byte) bool { return utf8.Valid(p) }
