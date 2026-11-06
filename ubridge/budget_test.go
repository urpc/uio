package ubridge

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/urpc/uio"
	"io"
	"net"
	"testing"
	"time"
)

// The hard write budget can be smaller than one source read. Both directions
// must make progress without dropping the suffix or moving FIN before it.
func TestBridgeRelaysChunksLargerThanDestinationBudget(t *testing.T) {
	for _, budget := range []int{1, 4, 7} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			upstream, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer upstream.Close()
			accepted := make(chan net.Conn, 1)
			acceptErr := make(chan error, 1)
			go func() {
				c, err := upstream.Accept()
				if err != nil {
					acceptErr <- err
					return
				}
				accepted <- c
			}()
			paired := make(chan struct{}, 1)
			started := make(chan struct{})
			br := &Bridge{Upstream: upstream.Addr().String(), InboundLimit: 64, OnPair: func(uio.Conn, uio.Conn) { paired <- struct{}{} }}
			br.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(started) }}
			ev := br.Events()
			ev.Pollers = 1
			ev.ReadBufferSize = 64
			ev.MaxOutboundBuffered = budget
			done := make(chan error, 1)
			go func() { done <- ev.Serve() }()
			t.Cleanup(func() { _ = ev.Close(nil); bridgeAwait(t, done, "bridge shutdown") })
			bridgeAwait(t, started, "bridge startup")
			listener, err := ev.Listen("tcp://127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			client, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			bridgeAwait(t, paired, "bridge pairing")
			var peer net.Conn
			select {
			case peer = <-accepted:
			case err := <-acceptErr:
				t.Fatal(err)
			case <-time.After(5 * time.Second):
				t.Fatal("upstream accept stalled")
			}
			defer peer.Close()
			_ = client.SetDeadline(time.Now().Add(5 * time.Second))
			_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
			forward := bytes.Repeat([]byte("forward-"), 33)
			if _, err := client.Write(forward); err != nil {
				t.Fatal(err)
			}
			if err := client.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if got, err := io.ReadAll(peer); err != nil || !bytes.Equal(got, forward) {
				t.Fatalf("forward got=%d/%d err=%v", len(got), len(forward), err)
			}
			reverse := bytes.Repeat([]byte("reverse-"), 35)
			if _, err := peer.Write(reverse); err != nil {
				t.Fatal(err)
			}
			if err := peer.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if got, err := io.ReadAll(client); err != nil || !bytes.Equal(got, reverse) {
				t.Fatalf("reverse got=%d/%d err=%v", len(got), len(reverse), err)
			}
		})
	}
}

type prefixWriteProbe struct {
	uio.Conn
	budget, queued, partial, calls int
	failure                        error
	written                        bytes.Buffer
}

func (p *prefixWriteProbe) OutboundBuffered() int { return p.queued }
func (p *prefixWriteProbe) Write(b []byte) (int, error) {
	p.calls++
	if p.partial > 0 {
		p.written.Write(b[:p.partial])
		return p.partial, p.failure
	}
	if p.failure != nil {
		return 0, p.failure
	}
	if len(b) > p.budget-p.queued {
		return 0, uio.ErrOutboundOverflow
	}
	p.written.Write(b)
	p.queued += len(b)
	return len(b), nil
}

func TestBridgePrefixAdmissionPreservesProgressAndFailures(t *testing.T) {
	payload := []byte("abcdefgh")
	for _, tc := range []struct {
		name                    string
		budget, queued, partial int
		failure                 error
		want                    int
		wantErr                 error
	}{
		{name: "small-budget", budget: 3, want: 2, wantErr: uio.ErrOutboundOverflow},
		{name: "capacity-reopened", budget: 8, want: 8},
		{name: "already-queued", budget: 4, queued: 1, wantErr: uio.ErrOutboundOverflow},
		{name: "zero-credit", wantErr: uio.ErrOutboundOverflow},
		{name: "accepted-prefix-with-error", partial: 3, failure: io.ErrClosedPipe, want: 3, wantErr: io.ErrClosedPipe},
		{name: "fatal-error", failure: io.ErrClosedPipe, wantErr: io.ErrClosedPipe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &prefixWriteProbe{budget: tc.budget, queued: tc.queued, partial: tc.partial, failure: tc.failure}
			n, err := (bridgePrefixWriter{probe}).Write(payload)
			if n != tc.want || !errors.Is(err, tc.wantErr) || !bytes.Equal(probe.written.Bytes(), payload[:n]) {
				t.Fatalf("n=%d err=%v written=%q", n, err, probe.written.Bytes())
			}
			if tc.name != "small-budget" && tc.name != "zero-credit" && probe.calls != 1 {
				t.Fatal("retried a queued destination or a write that already made progress")
			}
			if probe.calls > 4 {
				t.Fatal("prefix search did not terminate at one byte")
			}
		})
	}
}
