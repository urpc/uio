//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Drive the real OnOpen direct-send paths: OnRead intentionally corks its
// writes, so it cannot exercise direct vector/owned partial-write ownership.
func TestNativeDirectWritesPreservePayloadAndOverflowOwnership(t *testing.T) {
	for _, api := range []string{"Write", "Writev", "WriteOwned"} {
		for _, mode := range []string{"complete", "partial-queued", "partial-overflow", "refused-overflow", "refused-queued", "peer-closed"} {
			t.Run(api+"/"+mode, func(t *testing.T) {
				fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
				if err != nil {
					t.Fatal(err)
				}
				for _, fd := range fds {
					if err := unix.SetNonblock(fd, true); err != nil {
						t.Fatal(err)
					}
				}
				if err := unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 4<<10); err != nil {
					t.Fatal(err)
				}
				filled := 0
				if mode == "refused-overflow" || mode == "refused-queued" {
					filled = fillSocketUntilRefused(t, fds[0])
				}
				replacement := -1
				if mode == "peer-closed" {
					replacement, err = unix.Open("/dev/null", unix.O_RDONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = unix.Close(replacement) })
				}
				payload := bytes.Repeat([]byte("scatter-gather!"), 1<<16)
				if mode == "complete" || mode == "peer-closed" {
					payload = payload[:32]
				}
				want := bytes.Clone(payload)
				type result struct {
					n, buffered int
					err         error
					retained    bool
				}
				results := make(chan result, 1)
				closed := make(chan error, 1)
				ev := &Events{Pollers: 1}
				if mode == "partial-overflow" || mode == "refused-overflow" {
					ev.MaxOutboundBuffered = 1
				}
				funcs(ev).Open = func(c Conn) {
					if replacement >= 0 {
						// Atomically close the peer socket while keeping its numeric
						// descriptor occupied for the helper's eventual cleanup.
						if err := unix.Dup2(replacement, fds[1]); err != nil {
							t.Error(err)
						}
					}
					var n int
					var err error
					retained := false
					switch api {
					case "Write":
						n, err = c.Write(payload)
					case "Writev":
						cut := len(payload) / 2
						n, err = c.Writev([][]byte{nil, payload[:5], payload[5:cut], nil, payload[cut:]})
					case "WriteOwned":
						owned := AcquireBuffer(len(payload))
						_, _ = owned.Write(payload)
						n, err = c.WriteOwned(owned)
						if errors.Is(err, ErrOutboundOverflow) {
							retained = bytes.Equal(owned.Bytes(), want)
							ReleaseBuffer(owned) // only pure overflow returns ownership
						}
					}
					results <- result{n: n, err: err, buffered: c.OutboundBuffered(), retained: retained}
					// Ordinary Write/Writev must retain no caller memory, including
					// the suffix of a partial syscall queued for a later turn.
					clear(payload)
				}
				funcs(ev).Close = func(_ Conn, err error) { closed <- err }
				connection, registered := startTestConnectionOn(t, ev, fds)
				if err := <-registered; err != nil {
					t.Fatal(err)
				}
				got := readControlAwait(t, results, "direct callback write")
				switch mode {
				case "complete", "partial-queued", "refused-queued":
					if got.n != len(want) || got.err != nil {
						t.Fatalf("accepted write=%d,%v, want %d,nil", got.n, got.err, len(want))
					}
					if mode != "complete" && (got.buffered == 0 || got.buffered > len(want)) {
						t.Fatalf("partial send did not retain its suffix: buffered=%d", got.buffered)
					}
					if filled != 0 {
						_ = readPeerUntil(t, connection.peer, filled, 10*time.Second)
					}
					if wire := readPeerUntil(t, connection.peer, len(want), 10*time.Second); !bytes.Equal(wire, want) {
						t.Fatal("queued direct-send suffix retained or reordered caller memory")
					}
				case "partial-overflow":
					if got.n <= 0 || got.n >= len(want) || !errors.Is(got.err, io.ErrShortWrite) || errors.Is(got.err, ErrOutboundOverflow) {
						t.Fatalf("partial write=%d,%v, want positive short write", got.n, got.err)
					}
					if cause := readControlAwait(t, closed, "broken partial stream close"); !errors.Is(cause, ErrOutboundOverflow) {
						t.Fatalf("close=%v, want outbound overflow cause", cause)
					}
					if wire := readPeerUntil(t, connection.peer, got.n, 10*time.Second); !bytes.Equal(wire, want[:got.n]) {
						t.Fatal("short write did not report the exact prefix already sent")
					}
				case "refused-overflow":
					if got.n != 0 || !errors.Is(got.err, ErrOutboundOverflow) || got.buffered != 0 || connection.conn.isClosing() {
						t.Fatalf("refused write=%+v closing=%v", got, connection.conn.isClosing())
					}
					if api == "WriteOwned" && !got.retained {
						t.Fatal("a pure overflow consumed its caller-owned buffer")
					}
				case "peer-closed":
					if got.n != 0 || got.err == nil || errors.Is(got.err, ErrOutboundOverflow) {
						t.Fatalf("broken peer write=%d,%v", got.n, got.err)
					}
					if cause := readControlAwait(t, closed, "failed socket write close"); !errors.Is(cause, got.err) {
						t.Fatalf("write error %v was lost by close %v", got.err, cause)
					}
				}
			})
		}
	}
}

// Admission may first flush an earlier callback write to regain queue space.
// If that flush fails, every write form must report the transport error rather
// than an overflow, and the connection must retain its first close cause.
func TestNativeQueuedCallbackWritesPropagateFlushFailure(t *testing.T) {
	for _, api := range []string{"Write", "Writev", "WriteOwned"} {
		t.Run(api, func(t *testing.T) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			replacement, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(replacement)
			results := make(chan error, 1)
			closed := make(chan error, 1)
			events := withFuncs(&Events{Pollers: 1, WriteBufferedThreshold: 32, MaxOutboundBuffered: 8}, FuncHandler{
				Open: func(c Conn) {
					if n, err := c.Write([]byte("head")); n != 4 || err != nil || c.OutboundBuffered() != 4 {
						t.Errorf("queued first write=%d,%v; pending=%d", n, err, c.OutboundBuffered())
					}
					if err := unix.Dup2(replacement, fds[1]); err != nil {
						t.Error(err)
					}
					var n int
					var err error
					switch api {
					case "Write":
						n, err = c.Write([]byte("new-data"))
					case "Writev":
						n, err = c.Writev([][]byte{[]byte("new-"), []byte("data")})
					case "WriteOwned":
						owned := AcquireBuffer(8)
						_, _ = owned.WriteString("new-data")
						n, err = c.WriteOwned(owned)
					}
					if n != 0 || err == nil || errors.Is(err, ErrOutboundOverflow) {
						t.Errorf("write after failed admission flush=%d,%v", n, err)
					}
					results <- err
				},
				Close: func(_ Conn, err error) { closed <- err },
			})
			_, registered := startTestConnectionOn(t, events, fds)
			if err := <-registered; err != nil {
				t.Fatal(err)
			}
			cause := readControlAwait(t, results, "failed admission flush")
			if err := readControlAwait(t, closed, "failed flush closure"); !errors.Is(err, cause) {
				t.Fatalf("flush error %v lost as close cause %v", cause, err)
			}
		})
	}
}
