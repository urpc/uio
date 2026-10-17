package frame

import (
	"bytes"
	"fmt"
	"testing"
	"unicode/utf8"
)

// utf8Texts are valid UTF-8 runs of different shapes. Each starts with a
// different number of ASCII bytes, so the sequences after it straddle the
// 32-byte vector blocks at every phase.
func utf8Texts(size int) [][]byte {
	shapes := [][]string{
		{"中", "文", "文", "本"},          // three-byte, as Chinese text is
		{"é", "€", "😀"},               // two-, three- and four-byte
		{"😀", "a", "ß", "\U0010FFFF"}, // four-byte, up to the last code point
		// The first and last code point of each width, and those around
		// the surrogates.
		{"\u0080", "\u007f", "\u07ff", "\u0800", "\ud7ff", "\ue000", "\uffff", "\U00010000"},
	}
	var texts [][]byte
	for i, shape := range shapes {
		for lead := 0; lead < 4; lead++ {
			text := make([]byte, 0, size+8)
			text = append(text, "abc"[:lead]...)
			for j := i; len(text) < size; j++ {
				text = append(text, shape[j%len(shape)]...)
			}
			texts = append(texts, text)
		}
	}
	return texts
}

// utf8Bytes replace one byte of valid text: every lead and continuation class
// and the bytes at the edges of the ranges the tables check.
var utf8Bytes = []byte{0x00, 0x7f, 0x80, 0x8f, 0x90, 0x9f, 0xa0, 0xbf, 0xc0, 0xc1, 0xc2, 0xdf, 0xe0, 0xed, 0xef, 0xf0, 0xf4, 0xf5, 0xff}

// utf8Inserts are sequences planted whole: overlong, surrogate, out of range,
// truncated and stray ones, and valid ones at each range edge.
var utf8Inserts = [][]byte{
	{0xc0, 0xaf}, {0xc1, 0xbf}, {0xe0, 0x80, 0xaf}, {0xe0, 0x9f, 0xbf}, {0xf0, 0x80, 0x80, 0xaf}, {0xf0, 0x8f, 0xbf, 0xbf},
	{0xed, 0xa0, 0x80}, {0xed, 0xbf, 0xbf}, {0xf4, 0x90, 0x80, 0x80}, {0xf5, 0x80, 0x80, 0x80},
	{0xe4, 0xb8}, {0xf0, 0x9f, 0x98}, {0xc3}, {0x80}, {0xbf, 0xbf}, {0xff}, {0xfe, 0xff},
	{0xc2, 0x80}, {0xe0, 0xa0, 0x80}, {0xed, 0x9f, 0xbf}, {0xee, 0x80, 0x80}, {0xf0, 0x90, 0x80, 0x80}, {0xf4, 0x8f, 0xbf, 0xbf},
}

// utf8Sizes are the prefix lengths checked: every one up to 300, then around
// larger block multiples.
func utf8Sizes() []int {
	sizes := make([]int, 0, 330)
	for size := 0; size <= 300; size++ {
		sizes = append(sizes, size)
	}
	return append(sizes, 319, 320, 321, 511, 512, 513, 1023, 1024, 1025, 4095, 4096, 4097, 65539)
}

// mutated reports whether texts of this size also get every byte replacement
// and planted sequence: all sizes through two blocks and a bit, then those
// around each later block edge.
func mutated(size int) bool {
	if size <= 70 || size > 300 {
		return true
	}
	switch size % 32 {
	case 31, 0, 1:
		return true
	}
	return false
}

// utf8Positions are where a byte is replaced or a sequence planted:
// everywhere in a short text, and around each block edge, the middle and the
// tail of a long one.
func utf8Positions(size int) []int {
	if size <= 300 {
		positions := make([]int, size)
		for i := range positions {
			positions[i] = i
		}
		return positions
	}
	var positions []int
	for _, p := range []int{0, 1, 2, 3, 29, 30, 31, 32, 33, 63, 64, size / 2, size - 33, size - 32, size - 31, size - 4, size - 3, size - 2, size - 1} {
		if p >= 0 && p < size {
			positions = append(positions, p)
		}
	}
	return positions
}

// checkValidUTF8 holds ValidUTF8 to utf8.Valid on every prefix length of
// every text shape, which cuts sequences short at every point, and on those
// texts with each byte replaced and each sequence planted at every position.
// It also checks the input is left as it was.
func checkValidUTF8(t *testing.T) int {
	t.Helper()
	cases := 0
	// differs reports a mismatch with utf8.Valid or a changed input; the
	// caller says which case it was.
	scratch := make([]byte, 0, 1<<17)
	differs := func(p []byte) bool {
		cases++
		scratch = append(scratch[:0], p...)
		return ValidUTF8(p) != utf8.Valid(p) || !bytes.Equal(p, scratch)
	}
	report := func(p []byte, format string, args ...any) {
		t.Helper()
		t.Fatalf("%s (%d bytes): ValidUTF8 = %v, utf8.Valid = %v, input kept %v\n% x",
			fmt.Sprintf(format, args...), len(p), ValidUTF8(p), utf8.Valid(p), bytes.Equal(p, scratch), p)
	}
	sizes := utf8Sizes()
	texts := utf8Texts(sizes[len(sizes)-1] + 4)
	for ti, text := range texts {
		for _, size := range sizes {
			if differs(text[:size]) {
				report(text[:size], "text %d prefix", ti)
			}
			if !mutated(size) {
				continue
			}
			p := make([]byte, size)
			for _, at := range utf8Positions(size) {
				copy(p, text)
				for _, b := range utf8Bytes {
					p[at] = b
					if differs(p) {
						report(p, "text %d byte %#x at %d", ti, b, at)
					}
				}
				for _, seq := range utf8Inserts {
					if at+len(seq) > size {
						continue
					}
					copy(p, text)
					copy(p[at:], seq)
					if differs(p) {
						report(p, "text %d % x at %d", ti, seq, at)
					}
				}
			}
		}
	}
	return cases
}

func TestValidUTF8MatchesStdlib(t *testing.T) {
	t.Logf("%d cases (vector path from %d bytes: %v)", checkValidUTF8(t), vectorUTF8Min, useAVX2)
}

// Every sequence of up to three bytes, and every four-byte one built from
// the lead bytes and continuation edges, set across the first block edge and
// in the tail after it.
func TestValidUTF8MatchesStdlibOnEverySequence(t *testing.T) {
	if testing.Short() {
		t.Skip("exhaustive")
	}
	check := func(p []byte) {
		if got, want := ValidUTF8(p), utf8.Valid(p); got != want {
			t.Fatalf("ValidUTF8 = %v, utf8.Valid = %v\n% x", got, want, p)
		}
	}
	for _, size := range []int{64, 77} {
		for _, at := range []int{30, size - 3} {
			p := bytes.Repeat([]byte{'a'}, size)
			for v := 0; v < 1<<24; v++ {
				p[at], p[at+1], p[at+2] = byte(v>>16), byte(v>>8), byte(v)
				check(p)
			}
		}
	}
	edges := []byte{0x00, 0x7f, 0x80, 0x8f, 0x90, 0x9f, 0xa0, 0xbf, 0xc0, 0xff}
	p := bytes.Repeat([]byte("中"), 32)[:96]
	for lead := 0x80; lead <= 0xff; lead++ {
		for _, b1 := range edges {
			for _, b2 := range edges {
				for _, b3 := range edges {
					for at := 26; at <= 36; at++ {
						q := append([]byte(nil), p...)
						q[at], q[at+1], q[at+2], q[at+3] = byte(lead), b1, b2, b3
						check(q)
						check(q[:at+4])
					}
				}
			}
		}
	}
}

func FuzzValidUTF8(f *testing.F) {
	for _, s := range []string{"", "a", "中文文本", "😀é€", "\xff", "\xed\xa0\x80", "\xe4\xb8", "\xf4\x90\x80\x80"} {
		f.Add([]byte(s), uint8(0))
		f.Add([]byte(s), uint8(31))
	}
	f.Add(bytes.Repeat([]byte("中文"), 40), uint8(3))
	f.Fuzz(func(t *testing.T, p []byte, lead uint8) {
		// An ASCII or multibyte lead moves p across the block edges and
		// past the length where the vector path starts.
		prefix := bytes.Repeat([]byte("a"), int(lead%64))
		if lead >= 128 {
			prefix = bytes.Repeat([]byte("中"), int(lead%32))
		}
		for _, q := range [][]byte{p, append(prefix, p...)} {
			if got, want := ValidUTF8(q), utf8.Valid(q); got != want {
				t.Fatalf("ValidUTF8 = %v, utf8.Valid = %v\n% x", got, want, q)
			}
		}
	})
}

func BenchmarkValidUTF8(b *testing.B) {
	for _, multibyte := range []bool{false, true} {
		for _, size := range []int{32, 128, 1024, 4096} {
			unit := []byte(`{"k":"v"}`)
			if multibyte {
				unit = []byte("中文文本")
			}
			p := bytes.Repeat(unit, size/len(unit)+1)[:size/len(unit)*len(unit)]
			p = append(p, bytes.Repeat([]byte{'x'}, size-len(p))...)
			for _, impl := range []struct {
				name string
				f    func([]byte) bool
			}{{"ValidUTF8", ValidUTF8}, {"utf8.Valid", utf8.Valid}} {
				b.Run(fmt.Sprintf("multibyte=%v/%d/%s", multibyte, size, impl.name), func(b *testing.B) {
					b.SetBytes(int64(size))
					for range b.N {
						if !impl.f(p) {
							b.Fatal("valid text refused")
						}
					}
				})
			}
		}
	}
}
