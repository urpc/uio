//go:build linux || darwin

package frame

import (
	"testing"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// ValidUTF8 reads nothing outside its input and writes nothing: each case is
// laid against an inaccessible page, ending at one and starting right after
// another, in a page made read-only, so a stray load or store faults and
// takes the test binary down.
func TestValidUTF8StaysInsideItsInput(t *testing.T) {
	page := unix.Getpagesize()
	mem, err := unix.Mmap(-1, 0, 3*page, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		t.Skipf("mmap: %v", err)
	}
	defer unix.Munmap(mem)
	if err := unix.Mprotect(mem[:page], unix.PROT_NONE); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mprotect(mem[2*page:], unix.PROT_NONE); err != nil {
		t.Fatal(err)
	}
	mid := mem[page : 2*page]
	protect := func(prot int) {
		if err := unix.Mprotect(mid, prot); err != nil {
			t.Fatal(err)
		}
	}

	var sizes []int
	for _, size := range utf8Sizes() {
		if size <= page {
			sizes = append(sizes, size)
		}
	}
	sizes = append(sizes, page-1, page)
	cases := 0
	for ti, text := range utf8Texts(page) {
		for _, size := range sizes {
			valid := text[:size]
			variants := [][]byte{valid}
			if size > 0 {
				// A sequence cut short at the very end, and a stray
				// continuation byte at the very start.
				end := append([]byte(nil), valid...)
				end[size-1] = 0xe4
				start := append([]byte(nil), valid...)
				start[0] = 0x80
				variants = append(variants, end, start)
			}
			for vi, v := range variants {
				for _, atEnd := range []bool{true, false} {
					protect(unix.PROT_READ | unix.PROT_WRITE)
					p := mid[:size]
					if atEnd {
						p = mid[page-size:]
					}
					copy(p, v)
					protect(unix.PROT_READ)
					if got, want := ValidUTF8(p), utf8.Valid(p); got != want {
						t.Fatalf("text %d size %d variant %d at page end %v: ValidUTF8 = %v, utf8.Valid = %v", ti, size, vi, atEnd, got, want)
					}
					cases++
				}
			}
		}
	}
	protect(unix.PROT_READ | unix.PROT_WRITE)
	t.Logf("%d cases against guard pages of %d bytes", cases, page)
}
