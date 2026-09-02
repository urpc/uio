//go:build netbsd || dragonfly

package socket

import (
	"bytes"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWritevSocketpair(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	want := []byte("first-second-third")
	if n, err := Writev(fds[0], [][]byte{[]byte("first-"), nil, []byte("second-"), []byte("third")}); err != nil || n != len(want) {
		t.Fatalf("Writev() = %d, %v; want %d", n, err, len(want))
	}
	got := make([]byte, len(want))
	if _, err = unix.Read(fds[1], got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Writev payload = %q, want %q", got, want)
	}
}
