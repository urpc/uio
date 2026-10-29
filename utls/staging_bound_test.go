package utls

import (
	"crypto/tls"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// StagingBound paces the rounds; it does not cap one delivery. crypto/tls
// assembles a record from the bytes earlier rounds carried and hands it over
// whole when the last one arrives, so even a 1-byte read can deliver a full
// 16 KiB record: the peak is the bound plus about one record, not the size of
// the read that completed it.
func TestDeliveryPeaksPastTheStagingBound(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(map[uint16]string{tls.VersionTLS12: "tls1.2", tls.VersionTLS13: "tls1.3"}[version], func(t *testing.T) {
			serverConfig, clientConfig := testConfigs(t)
			serverConfig.MaxVersion = version
			clientConfig.MaxVersion = version
			const total = 3 * (16 << 10)
			payload := randomBytes(t, total)

			var received, maxDelivery atomic.Int64
			closed := make(chan error, 1)
			top := uio.FuncHandler{
				Read: func(c uio.Conn) error {
					n := c.InboundBuffered()
					if int64(n) > maxDelivery.Load() {
						maxDelivery.Store(int64(n))
					}
					received.Add(int64(n))
					_, _ = c.Discard(n)
					return nil
				},
				Close: func(_ uio.Conn, err error) { signal(closed, err) },
			}
			// One-byte socket reads against a one-byte bound: every record is
			// assembled across thousands of rounds.
			transport := serveTLSWith(t, top, func(ev *uio.Events) { ev.MaxBufferSize = 1 },
				Server(serverConfig, StagingBound(1)))
			listener, err := transport.Listen("tcp://127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			conn, _ := dialTLS(t, Client(clientConfig), "tcp://"+listener.Addr().String())
			if _, err := conn.Write(payload); err != nil {
				t.Fatal(err)
			}
			if err := conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(testWait)
			for received.Load() < total && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := received.Load(); got != total {
				t.Fatalf("delivered %d of %d bytes", got, total)
			}
			select {
			case err := <-closed:
				if err != nil && !errors.Is(err, io.EOF) {
					t.Fatalf("closed with %v, want the peer's clean end", err)
				}
			case <-time.After(testWait):
				t.Fatal("the end of the stream never came")
			}
			if peak := maxDelivery.Load(); peak > 1+1+(16<<10) {
				t.Fatalf("largest delivery = %d, want at most the bound plus a chunk plus one record", peak)
			}
			// crypto/tls may size records below 16 KiB — newer Go autotunes
			// them down — so the floor only asks that one whole record went
			// past the bound of one byte.
			if peak := maxDelivery.Load(); peak < 4<<10 {
				t.Fatalf("largest delivery = %d, expected a whole record past a bound of 1", peak)
			}
		})
	}
}
