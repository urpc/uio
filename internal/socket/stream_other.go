//go:build (linux && race) || darwin || netbsd || freebsd || openbsd || dragonfly

package socket

import "syscall"

// Recv reads from a connected stream socket. Race builds keep the syscall
// package's race annotations, and the BSDs keep read.
func Recv(fd int, p []byte) (int, error) { return syscall.Read(fd, p) }

// Send writes to a connected stream socket.
func Send(fd int, p []byte) (int, error) { return syscall.Write(fd, p) }
