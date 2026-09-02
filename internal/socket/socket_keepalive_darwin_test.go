//go:build darwin

package socket

import (
	"golang.org/x/sys/unix"
	"testing"
)

func TestDarwinKeepAlivePeriodRejectsNonPositive(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := SetKeepAlivePeriod(fd, 0); err == nil {
		t.Fatal("SetKeepAlivePeriod(0) succeeded")
	}
}
