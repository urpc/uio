//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

// stdioBackend tells cross-backend tests which backend they run on.
const stdioBackend = false
