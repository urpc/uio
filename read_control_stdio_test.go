//go:build windows || stdio

package uio

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// A released pause's resume turn — a Wake — can overtake OnOpen: deliverWake
// stores it for replay. Pin that ordering before registration so both the
// replay and the first socket read are verified without depending on
// scheduler timing.
func TestReadControlResumeBeforeOnOpenStartsSocketReads(t *testing.T) {
	for _, layered := range []bool{false, true} {
		name := "plain"
		if layered {
			name = "layered"
		}
		t.Run(name, func(t *testing.T) {
			raw := newStdRegistrationConn(30109)
			replayed := make(chan struct{}, 1)
			data := make(chan string, 1)
			events := &Events{Pollers: 1, Handler: FuncHandler{
				Open: func(Conn) { raw.openReturned.Store(true) },
				Read: func(c Conn) error {
					if c.InboundBuffered() == 0 {
						select {
						case replayed <- struct{}{}:
						default:
						}
						return nil
					}
					buffer := make([]byte, c.InboundBuffered())
					_, err := c.Read(buffer)
					data <- string(buffer)
					return err
				},
			}}
			done := startStdTestEvents(t, events)
			t.Cleanup(func() {
				_ = raw.Close()
				stopStdTestEvents(t, events, done, nil)
			})
			conn := newStdFDConn(events, raw)
			conn.readResume = make(chan struct{}, 1)
			if layered {
				entry, err := events.entryFor([]Layer{PassThroughLayer}, true)
				if err != nil {
					t.Fatal(err)
				}
				conn.entry = entry
			}
			if err := conn.PauseReads(); err != nil {
				t.Fatal(err)
			}
			if err := conn.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 5*time.Second, func() bool {
				conn.callbackMu.Lock()
				defer conn.callbackMu.Unlock()
				return conn.wakesAfterOpen != 0
			})
			if err := events.addConn(conn); err != nil {
				t.Fatal(err)
			}
			readControlAwait(t, replayed, "the pre-open resume replay")
			select {
			case raw.readData <- []byte("first-input"):
			case <-time.After(5 * time.Second):
				t.Fatal("pre-open resume left the first socket read blocked")
			}
			if got := readControlAwait(t, data, "input after the replayed resume"); got != "first-input" {
				t.Fatalf("received %q, want first-input", got)
			}
		})
	}
}

// Unlike orderly EOF, a fatal transport failure aborts independently of an
// application pause. Waiting for Resume here can strand both cleanup and the
// application that is waiting to hear why its transport stopped.
func TestReadControlInFlightFatalReadErrorClosesWhilePaused(t *testing.T) {
	for _, payload := range []string{"", "discarded-on-abort"} {
		name := "no-payload"
		if payload != "" {
			name = "with-payload"
		}
		t.Run(name, func(t *testing.T) {
			cause := errors.New("test: fatal transport read error")
			raw := newStdRegistrationConn(30108)
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			raw.readFunc = func(p []byte) (int, error) {
				once.Do(func() { close(entered) })
				select {
				case <-release:
					return copy(p, payload), cause
				case <-raw.closed:
					return 0, net.ErrClosed
				}
			}
			closed := make(chan error, 1)
			ev := &Events{Pollers: 1, Handler: FuncHandler{
				Open:  func(Conn) { raw.openReturned.Store(true) },
				Read:  func(Conn) error { return nil },
				Close: func(_ Conn, err error) { closed <- err },
			}}
			done := startStdTestEvents(t, ev)
			t.Cleanup(func() { stopStdTestEvents(t, ev, done, nil) })
			server, err := ev.Adopt(raw)
			if err != nil {
				t.Fatal(err)
			}
			readControlAwait(t, entered, "the in-flight Read")
			if err := server.PauseReads(); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := readControlAwait(t, closed, "fatal error closing the paused connection"); !errors.Is(err, cause) {
				t.Fatalf("OnClose cause = %v, want %v", err, cause)
			}
		})
	}
}

// A blocking Read cannot be canceled merely by publishing a pause. Its final
// payload and simultaneous EOF must survive until the paused consumer resumes.
func TestReadControlInFlightReadRetainsPayloadAndEOF(t *testing.T) {
	for _, layered := range []bool{false, true} {
		name := "plain"
		var opts []AdoptOption
		if layered {
			name = "layered"
			opts = []AdoptOption{WithLayers(PassThroughLayer, PassThroughLayer)}
		}
		t.Run(name, func(t *testing.T) {
			raw := newStdRegistrationConn(30107)
			entered := make(chan struct{})
			release := make(chan struct{})
			returned := make(chan struct{})
			var once sync.Once
			raw.readFunc = func(p []byte) (int, error) {
				first := false
				once.Do(func() { first = true; close(entered) })
				if !first {
					return 0, io.EOF
				}
				select {
				case <-release:
					defer close(returned)
					return copy(p, "last-payload"), io.EOF
				case <-raw.closed:
					return 0, net.ErrClosed
				}
			}
			reads := make(chan string, 8)
			closed := make(chan error, 1)
			ev := &Events{Pollers: 1, Handler: FuncHandler{
				Open: func(Conn) { raw.openReturned.Store(true) },
				Read: func(c Conn) error {
					if n := c.InboundBuffered(); n != 0 {
						p := make([]byte, n)
						_, _ = c.Read(p)
						reads <- string(p)
					}
					return nil
				},
				Close: func(_ Conn, err error) { closed <- err },
			}}
			done := startStdTestEvents(t, ev)
			t.Cleanup(func() { stopStdTestEvents(t, ev, done, nil) })
			server, err := ev.Adopt(raw, opts...)
			if err != nil {
				t.Fatal(err)
			}
			readControlAwait(t, entered, "the in-flight Read")
			if err := server.PauseReads(); err != nil {
				t.Fatal(err)
			}
			close(release)
			readControlAwait(t, returned, "Read returning its final payload and EOF")
			if err := server.Wake(); err != nil {
				t.Fatal(err)
			}
			readControlQuiet(t, reads, "in-flight input delivery before resume")
			readControlQuiet(t, closed, "EOF closing before the paused payload was delivered")
			if err := server.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			if got := readControlAwait(t, reads, "in-flight final payload after resume"); got != "last-payload" {
				t.Fatalf("final payload = %q, want last-payload", got)
			}
			readControlAwait(t, closed, "EOF after final payload delivery")
		})
	}
}
