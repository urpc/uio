package umux

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ukcp"
	"github.com/urpc/uio/utls"
)

func selfSignedTLSConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	certObj, _ := x509.ParseCertificate(der)
	pool.AddCert(certObj)
	srvCfg := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
	}
	cliCfg := &tls.Config{
		RootCAs:    pool,
		ServerName: "127.0.0.1",
	}
	return srvCfg, cliCfg
}

func TestBlockingOverTLSAndUmuxAcceptAndHandlerModes(t *testing.T) {
	srvTLS, cliTLS := selfSignedTLSConfigs(t)

	for _, handlerMode := range []bool{false, true} {
		modeName := "accept_mode"
		if handlerMode {
			modeName = "handler_mode"
		}
		t.Run(modeName, func(t *testing.T) {
			srvStreamBC := make(chan *uio.BlockingConn, 1)

			var srvUmuxCfg Config
			srvUmuxCfg.WindowSize = 256 << 10
			if handlerMode {
				srvUmuxCfg.Handler = uio.FuncHandler{
					Open: func(c uio.Conn) {
						bc, err := uio.NewBlockingConn(c, uio.WithBufferSize(16<<10))
						if err != nil {
							t.Errorf("umux server NewBlockingConn: %v", err)
							return
						}
						if _, err := bc.Read(make([]byte, 4)); !errors.Is(err, uio.ErrBlockingOnTurn) {
							t.Errorf("umux OnOpen Read = %v, want ErrBlockingOnTurn", err)
						}
						srvStreamBC <- bc
					},
				}
			}

			srv := NewServer(srvUmuxCfg)
			defer func() {
				_ = srv.Close(nil)
				_ = srv.Wait()
			}()

			ln, err := srv.Listen("tcp://127.0.0.1:0", uio.WithLayers(utls.ServerLayer(utls.LayerOptions{TLSConfig: srvTLS})))
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()

			cli := NewClient(Config{WindowSize: 256 << 10})
			defer func() {
				_ = cli.Close(nil)
			}()

			cliSess, err := cli.Dial(context.Background(), "tcp://"+ln.Addr().String(), uio.WithLayers(utls.ClientLayer(utls.LayerOptions{TLSConfig: cliTLS})))
			if err != nil {
				t.Fatal(err)
			}
			defer cliSess.Close(nil)

			if !handlerMode {
				go func() {
					srvSess := <-srv.Sessions()
					if srvSess == nil {
						return
					}
					acc, ok := <-srvSess.Accept()
					if !ok {
						return
					}
					bc, err := uio.NewBlockingConn(acc.Stream, uio.WithBufferSize(16<<10))
					if err != nil {
						t.Errorf("accept NewBlockingConn: %v", err)
						return
					}
					srvStreamBC <- bc
				}()
			}

			cliSt, err := cliSess.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cliBC, err := uio.NewBlockingConn(cliSt, uio.WithBufferSize(16<<10))
			if err != nil {
				t.Fatal(err)
			}
			srvBC := <-srvStreamBC

			// Verify deadline expiry does not close the multiplexed stream!
			_ = cliBC.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			if _, err := cliBC.Read(make([]byte, 8)); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("umux deadline Read = %v", err)
			}
			_ = cliBC.SetReadDeadline(time.Time{})

			// Stream 768 KiB across 256 KiB umux window + 16 KiB blocking pipe!
			payload := make([]byte, 768<<10)
			_, _ = rand.Read(payload)

			echoDone := make(chan error, 1)
			go func() {
				_, err := io.Copy(srvBC, srvBC)
				if err == nil {
					err = srvBC.Flush()
				}
				if err == nil {
					err = srvBC.CloseWrite()
				}
				echoDone <- err
			}()

			sendDone := make(chan error, 1)
			go func() {
				_, err := cliBC.Write(payload)
				if err == nil {
					err = cliBC.Flush()
				}
				if err == nil {
					err = cliBC.CloseWrite()
				}
				sendDone <- err
			}()

			got, err := io.ReadAll(cliBC)
			if err != nil {
				t.Fatalf("cliBC ReadAll = %v", err)
			}
			if err := <-sendDone; err != nil {
				t.Fatalf("cliBC send = %v", err)
			}
			if err := <-echoDone; err != nil {
				t.Fatalf("srvBC echo = %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("TLS+umux echoed %d bytes mismatch", len(got))
			}

			// Test local Abort waking parked reader on a second stream!
			st2, err := cliSess.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			bc2, _ := uio.NewBlockingConn(st2)
			abortWait := make(chan error, 1)
			abortCause := errors.New("reset-now")
			go func() {
				_, err := bc2.Read(make([]byte, 8))
				abortWait <- err
			}()
			time.Sleep(15 * time.Millisecond)
			_ = bc2.Abort(abortCause)
			if err := <-abortWait; !errors.Is(err, net.ErrClosed) {
				t.Fatalf("umux Abort Read = %v, want net.ErrClosed", err)
			}
		})
	}
}

func TestBlockingOverKCPLayerAndUmux(t *testing.T) {
	kcpOpts := ukcp.Options{NoDelay: true, Interval: 10 * time.Millisecond}
	srvStreamBC := make(chan *uio.BlockingConn, 1)

	srv := NewServer(Config{
		Handler: uio.FuncHandler{
			Open: func(c uio.Conn) {
				bc, err := uio.NewBlockingConn(c, uio.WithBufferSize(16<<10))
				if err != nil {
					t.Errorf("kcp+umux NewBlockingConn: %v", err)
					return
				}
				srvStreamBC <- bc
			},
		},
	})
	defer func() {
		_ = srv.Close(nil)
		_ = srv.Wait()
	}()

	ln, err := srv.Listen("udp://127.0.0.1:0", uio.WithLayers(ukcp.ServerLayer(kcpOpts)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cli := NewClient(Config{})
	defer func() {
		_ = cli.Close(nil)
	}()

	cliSess, err := cli.Dial(context.Background(), "udp://"+ln.Addr().String(), uio.WithLayers(ukcp.ClientLayer(kcpOpts)))
	if err != nil {
		t.Fatal(err)
	}
	defer cliSess.Close(nil)

	cliSt, err := cliSess.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cliBC, err := uio.NewBlockingConn(cliSt, uio.WithBufferSize(16<<10))
	if err != nil {
		t.Fatal(err)
	}
	srvBC := <-srvStreamBC

	payload := bytes.Repeat([]byte("kcp-umux-blockio-"), 16384) // ~278 KiB (> 256 KiB window!)
	go func() {
		_, _ = io.Copy(srvBC, srvBC)
		_ = srvBC.CloseWrite()
	}()
	go func() {
		_, _ = cliBC.Write(payload)
		_ = cliBC.CloseWrite()
	}()

	got, err := io.ReadAll(cliBC)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("KCP+umux ReadAll len=%d err=%v", len(got), err)
	}
}

func TestUmuxConfigBlockingWriteOption(t *testing.T) {
	srvStreamCh := make(chan *Stream, 1)
	onTurnOverflow := make(chan error, 1)

	srv := NewServer(Config{
		BlockingWrite: true,
		Handler: uio.FuncHandler{
			Open: func(c uio.Conn) {
				st := c.(*Stream)
				if st.ID() == 1 {
					// Calling Open or Shutdown on this session's own turn returns ErrBlockingOnTurn!
					if _, oerr := st.sess.Open(context.Background()); !errors.Is(oerr, uio.ErrBlockingOnTurn) {
						t.Errorf("own-turn Open = %v", oerr)
					}
					if serr := st.sess.Shutdown(context.Background()); !errors.Is(serr, uio.ErrBlockingOnTurn) {
						t.Errorf("own-turn Shutdown = %v", serr)
					}
					// On-turn overflow (> 256 KiB window) MUST return ErrOutboundOverflow without deadlocking!
					_, err := c.Write(make([]byte, 512<<10))
					onTurnOverflow <- err
					srvStreamCh <- st
				}
			},
		},
	})
	defer func() {
		_ = srv.Close(nil)
		_ = srv.Wait()
	}()

	ln, err := srv.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cli := NewClient(Config{BlockingWrite: true})
	defer func() { _ = cli.Close(nil) }()

	cliSess, err := cli.Dial(context.Background(), "tcp://"+ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cliSess.Close(nil)

	cliSt, err := cliSess.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if err := <-onTurnOverflow; !errors.Is(err, uio.ErrOutboundOverflow) {
		t.Fatalf("umux on-turn overflow = %v, want ErrOutboundOverflow", err)
	}
	srvSt := <-srvStreamCh
	srvR, err := uio.NewBlockingReader(srvSt)
	if err != nil {
		t.Fatal(err)
	}

	// Drain initial partial bytes accepted during the on-turn test on cliSt.
	cliR, err := uio.NewBlockingReader(cliSt)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, cliR) }()

	// Business goroutine writes 600 KiB (> 256 KiB stream window!) directly via Stream.Write, Writev, and WriteOwned!
	p1 := bytes.Repeat([]byte("UMUX-BLK-1-"), 30000) // 330 KiB
	p2 := bytes.Repeat([]byte("UMUX-BLK-2-"), 20000) // 220 KiB
	p3 := bytes.Repeat([]byte("UMUX-BLK-3-"), 10000) // 110 KiB

	writeDone := make(chan error, 1)
	go func() {
		if _, err := cliSt.Write(p1); err != nil {
			writeDone <- err
			return
		}
		if _, err := cliSt.Writev([][]byte{p2[:100000], p2[100000:]}); err != nil {
			writeDone <- err
			return
		}
		owned := uio.AcquireBuffer(len(p3))
		_, _ = owned.Write(p3)
		if _, err := cliSt.WriteOwned(owned); err != nil {
			writeDone <- err
			return
		}
		writeDone <- cliSt.CloseWrite()
	}()

	got, err := io.ReadAll(srvR)
	if err != nil {
		t.Fatalf("srvR ReadAll = %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("cliSt BlockingWrite = %v", err)
	}
	want := bytes.Join([][]byte{p1, p2, p3}, nil)
	if !bytes.Equal(got, want) {
		t.Fatalf("umux BlockingWrite received %d bytes, want %d", len(got), len(want))
	}

	// Verify Stream.Close() wakes parked BlockingConn.Read immediately and Session.Open/Shutdown reject own-turn!
	st2, err := cliSess.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bc2, err := uio.NewBlockingConn(st2)
	if err != nil {
		t.Fatal(err)
	}
	readErrCh := make(chan error, 1)
	go func() {
		_, rerr := bc2.Read(make([]byte, 16))
		readErrCh <- rerr
	}()
	time.Sleep(15 * time.Millisecond)
	if err := bc2.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case rerr := <-readErrCh:
		if !errors.Is(rerr, net.ErrClosed) {
			t.Fatalf("parked Read after Stream.Close = %v, want net.ErrClosed", rerr)
		}
	case <-time.After(time.Second):
		t.Fatal("Stream.Close did not wake parked BlockingConn.Read")
	}

	// Attaching NewBlockingConn to an already-closed Stream immediately observes close!
	bcClosed, err := uio.NewBlockingConn(st2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bcClosed.Read(make([]byte, 8)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("pre-closed Stream BlockingConn.Read = %v, want net.ErrClosed", err)
	}
}
