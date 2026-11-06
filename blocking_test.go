/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package uio

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingDecoratedConn struct{ Conn }

func (d blockingDecoratedConn) Unwrap() Conn { return d.Conn }

type asyncTestExecutor struct{}

func (asyncTestExecutor) Submit(task IOTask) bool {
	go task.RunTask()
	return true
}

func (asyncTestExecutor) SubmitBatch(tasks []IOTask) int {
	for _, task := range tasks {
		go task.RunTask()
	}
	return len(tasks)
}

func startBlockingEvents(t *testing.T, ev *Events) func() {
	t.Helper()
	started := make(chan struct{})
	ev.Hooks = FuncHandler{
		Start: func(*Events) { close(started) },
	}
	done := make(chan error, 1)
	go func() { done <- ev.Serve() }()
	<-started
	return func() {
		_ = ev.Close(nil)
		<-done
	}
}

func runBlockingPair(
	t *testing.T,
	layered bool,
	halfOpen bool,
	maxIn, maxOut int,
	exec Executor,
	onServer func(Conn),
) (client Conn, cleanup func()) {
	t.Helper()
	var listenOpts []ListenOption
	var adoptOpts []AdoptOption
	if layered {
		listenOpts = []ListenOption{WithLayers(PassThroughLayer, PassThroughLayer)}
		adoptOpts = []AdoptOption{WithLayers(PassThroughLayer, PassThroughLayer)}
	}

	ev := &Events{
		Pollers:             1,
		AllowHalfOpen:       halfOpen,
		MaxInboundBuffered:  maxIn,
		MaxOutboundBuffered: maxOut,
		Executor:            exec,
		Handler: FuncHandler{
			Open: func(c Conn) {
				if onServer != nil {
					onServer(c)
				}
			},
		},
	}
	stopEv := startBlockingEvents(t, ev)

	ln, err := ev.Listen("tcp://127.0.0.1:0", listenOpts...)
	if err != nil {
		stopEv()
		t.Fatal(err)
	}

	rawNet, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		stopEv()
		t.Fatal(err)
	}
	cliOpened := make(chan struct{})
	cliEv := &Events{
		Pollers:             1,
		AllowHalfOpen:       halfOpen,
		MaxInboundBuffered:  maxIn,
		MaxOutboundBuffered: maxOut,
		Handler: FuncHandler{
			Open: func(Conn) { close(cliOpened) },
		},
	}
	stopCliEv := startBlockingEvents(t, cliEv)
	cli, err := cliEv.Adopt(rawNet, adoptOpts...)
	if err != nil {
		_ = ln.Close()
		stopCliEv()
		stopEv()
		t.Fatal(err)
	}
	<-cliOpened

	return cli, func() {
		_ = cli.Close()
		_ = ln.Close()
		stopCliEv()
		stopEv()
	}
}

func TestBlockingUnsupportedAndDecoratorUnwrap(t *testing.T) {
	if _, err := NewBlockingReader(nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nil NewBlockingReader = %v", err)
	}
	if _, err := NewBlockingWriter(nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nil NewBlockingWriter = %v", err)
	}
	if _, err := NewBlockingConn(nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nil NewBlockingConn = %v", err)
	}

	ev := &Events{Pollers: 1, Handler: FuncHandler{}}
	stop := startBlockingEvents(t, ev)
	defer stop()
	ln, err := ev.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	udpConn, err := ev.Dial("udp://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer udpConn.Close()
	if _, err := NewBlockingReader(udpConn); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("UDP NewBlockingReader = %v", err)
	}
	if _, err := NewBlockingWriter(udpConn); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("UDP NewBlockingWriter = %v", err)
	}
	if _, err := NewBlockingConn(udpConn); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("UDP NewBlockingConn = %v", err)
	}
}

func TestBlockingConformanceAcrossPlainAndLayeredStacks(t *testing.T) {
	for _, layered := range []bool{false, true} {
		name := "plain"
		if layered {
			name = "layered"
		}
		t.Run(name, func(t *testing.T) {
			srvReady := make(chan *BlockingConn, 1)
			cliRaw, cleanup := runBlockingPair(t, layered, true, 16<<10, 16<<10, nil, func(c Conn) {
				bc, err := NewBlockingConn(blockingDecoratedConn{Conn: c}, WithBufferSize(8<<10))
				if err != nil {
					t.Errorf("server NewBlockingConn: %v", err)
					return
				}
				if _, err := bc.Read(make([]byte, 4)); !errors.Is(err, ErrBlockingOnTurn) {
					t.Errorf("OnOpen Read = %v, want ErrBlockingOnTurn", err)
				}
				if _, err := bc.Write([]byte("x")); !errors.Is(err, ErrBlockingOnTurn) {
					t.Errorf("OnOpen Write = %v, want ErrBlockingOnTurn", err)
				}
				srvReady <- bc
			})
			defer cleanup()

			cliBC, err := NewBlockingConn(cliRaw, WithBufferSize(8<<10))
			if err != nil {
				t.Fatal(err)
			}
			srvBC := <-srvReady

			if cliBC.Unwrap() != cliRaw || cliBC.LocalAddr() == nil || cliBC.RemoteAddr() == nil {
				t.Fatal("BlockingConn metadata/unwrap mismatch")
			}
			if cliBC.Reader() == nil || cliBC.Writer() == nil {
				t.Fatal("Reader/Writer accessors nil")
			}

			// 1. Deadlines interrupt blocked waits without closing the connection!
			if err := cliBC.SetDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if _, err := cliBC.Read(make([]byte, 8)); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("deadline Read = %v", err)
			}
			_ = cliBC.SetDeadline(time.Time{})

			// 2. Large 256 KiB bidirectional backpressured stream + ReadFrom + zero-copy WriteTo + Half-Close!
			payload := make([]byte, 256<<10)
			_, _ = rand.Read(payload)

			srvErr := make(chan error, 1)
			go func() {
				_, err := io.Copy(srvBC.Writer(), srvBC.Reader())
				if err == nil {
					err = srvBC.Flush()
				}
				if err == nil {
					err = srvBC.CloseWrite()
				}
				srvErr <- err
			}()

			writeErr := make(chan error, 1)
			go func() {
				// Exercise both Write (slice) and ReadFrom (pooled WriteOwned) across plain & layered stacks!
				half := len(payload) / 2
				n1, err := cliBC.Write(payload[:half])
				if err == nil && n1 != half {
					err = io.ErrShortWrite
				}
				if err == nil {
					var n2 int64
					n2, err = cliBC.ReadFrom(bytes.NewReader(payload[half:]))
					if err == nil && n2 != int64(len(payload)-half) {
						err = io.ErrShortWrite
					}
				}
				if err == nil {
					err = cliBC.Writer().Flush()
				}
				if err == nil {
					err = cliBC.CloseWrite()
				}
				writeErr <- err
			}()

			got, err := io.ReadAll(cliBC)
			if err != nil {
				t.Fatalf("client ReadAll = %v", err)
			}
			if err := <-writeErr; err != nil {
				t.Fatalf("client write = %v", err)
			}
			if err := <-srvErr; err != nil {
				t.Fatalf("server echo = %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("echoed %d bytes did not match sent %d bytes", len(got), len(payload))
			}

			if _, err := cliBC.Write([]byte("late")); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("post-CloseWrite Write = %v", err)
			}
		})
	}
}

func TestBlockingMidStreamUpgradeAndStdlibHTTP(t *testing.T) {
	httpDone := make(chan error, 1)
	var upgraded sync.Map

	ev := &Events{
		Pollers:       1,
		AllowHalfOpen: true,
		Executor:      asyncTestExecutor{},
		Handler: FuncHandler{
			Read: func(c Conn) error {
				if _, ok := upgraded.Load(c); ok {
					t.Errorf("OnRead called again after NewBlockingReader takeover")
					return nil
				}
				chunk := c.PeekChunk()
				idx := bytes.Index(chunk, []byte("\n"))
				if idx < 0 {
					return nil
				}
				line := string(chunk[:idx])
				_, _ = c.Discard(idx + 1)
				if line != "PREFACE" {
					return errors.New("bad preface")
				}
				upgraded.Store(c, true)
				bc, err := NewBlockingConn(c)
				if err != nil {
					return err
				}
				if _, err := bc.Read(make([]byte, 1)); !errors.Is(err, ErrBlockingOnTurn) {
					t.Errorf("Executor OnRead Read = %v, want ErrBlockingOnTurn", err)
				}
				go func() {
					defer bc.Close()
					req, err := http.ReadRequest(bufio.NewReader(bc))
					if err != nil {
						httpDone <- err
						return
					}
					body, err := io.ReadAll(req.Body)
					_ = req.Body.Close()
					if err != nil {
						httpDone <- err
						return
					}
					resp := &http.Response{
						StatusCode:    http.StatusOK,
						ProtoMajor:    1,
						ProtoMinor:    1,
						ContentLength: int64(len(body)),
						Body:          io.NopCloser(bytes.NewReader(bytes.ToUpper(body))),
					}
					httpDone <- resp.Write(bc)
				}()
				return nil
			},
		},
	}

	stop := startBlockingEvents(t, ev)
	defer stop()
	ln, err := ev.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	rawCli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer rawCli.Close()

	reqPayload := "PREFACE\nPOST /echo HTTP/1.1\r\nHost: example.com\r\nContent-Length: 11\r\n\r\nhello world"
	if _, err := io.WriteString(rawCli, reqPayload); err != nil {
		t.Fatal(err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(rawCli), nil)
	if err != nil {
		t.Fatal(err)
	}
	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(respBody) != "HELLO WORLD" {
		t.Fatalf("HTTP response = %q, %v", respBody, err)
	}
	if err := <-httpDone; err != nil {
		t.Fatal(err)
	}
}

func TestBlockingLocalCloseAndAbortWakeParkedCallers(t *testing.T) {
	srvConnCh := make(chan *BlockingConn, 1)
	_, cleanup1 := runBlockingPair(t, false, false, 0, 0, nil, func(c Conn) {
		bc, _ := NewBlockingConn(c)
		srvConnCh <- bc
	})
	srvBC1 := <-srvConnCh
	readErrCh := make(chan error, 1)
	go func() {
		_, err := srvBC1.Read(make([]byte, 16))
		readErrCh <- err
	}()
	time.Sleep(15 * time.Millisecond)
	_ = srvBC1.Close()
	if err := <-readErrCh; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("local Close parked Read = %v, want net.ErrClosed", err)
	}
	cleanup1()

	abortCause := errors.New("abort-stream")
	srvConnCh2 := make(chan *BlockingConn, 1)
	_, cleanup2 := runBlockingPair(t, true, false, 0, 0, nil, func(c Conn) {
		bc, _ := NewBlockingConn(c)
		srvConnCh2 <- bc
	})
	srvBC2 := <-srvConnCh2
	abortErrCh := make(chan error, 1)
	go func() {
		_, err := srvBC2.Read(make([]byte, 16))
		abortErrCh <- err
	}()
	time.Sleep(15 * time.Millisecond)
	_ = srvBC2.Abort(abortCause)
	if err := <-abortErrCh; !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "abort") {
		t.Fatalf("Abort parked Read = %v", err)
	}
	cleanup2()

	srvConnCh3 := make(chan *BlockingConn, 1)
	cli3, cleanup3 := runBlockingPair(t, false, false, 0, 0, nil, func(c Conn) {
		bc, _ := NewBlockingConn(c)
		srvConnCh3 <- bc
	})
	defer cleanup3()
	cliBC3, _ := NewBlockingConn(cli3)
	srvBC3 := <-srvConnCh3
	_ = cliBC3.SetReadDeadline(time.Time{})
	_ = cliBC3.SetWriteDeadline(time.Time{})
	owned := AcquireBuffer(16)
	_, _ = owned.WriteString("owned-msg")
	if n, err := cliBC3.Writer().WriteOwned(owned); err != nil || n != 9 {
		t.Fatalf("Writer.WriteOwned = %d, %v", n, err)
	}
	_ = cliBC3.Reader().Buffered()
	_ = cliBC3.Writer().Buffered()
	got := make([]byte, 9)
	if _, err := io.ReadFull(srvBC3, got); err != nil || string(got) != "owned-msg" {
		t.Fatalf("ReadFull owned = %q, %v", got, err)
	}
}

func TestEventsBlockingWriteOptionFairnessTurnGuardAndDeadline(t *testing.T) {
	// 1. Default MaxOutboundBuffered == 0 converges to DefaultBlockingBuffer when BlockingWrite: true.
	defEv := &Events{BlockingWrite: true}
	if defEv.effectiveMaxOutbound() != DefaultBlockingBuffer {
		t.Fatalf("effectiveMaxOutbound = %d, want %d", defEv.effectiveMaxOutbound(), DefaultBlockingBuffer)
	}

	for _, layered := range []bool{false, true} {
		name := "plain"
		if layered {
			name = "layered"
		}
		t.Run(name, func(t *testing.T) {
			var opts []ListenOption
			var dialOpts []DialOption
			if layered {
				opts = []ListenOption{WithLayers(PassThroughLayer)}
				dialOpts = []DialOption{WithLayers(PassThroughLayer)}
			}

			srvReaderCh := make(chan *BlockingReader, 1)
			turnOverflowCh := make(chan error, 1)

			srvEv := &Events{
				Pollers:             1,
				AllowHalfOpen:       true,
				BlockingWrite:       true,
				MaxOutboundBuffered: 8 << 10,
				Handler: FuncHandler{
					Open: func(c Conn) {
						r, err := NewBlockingReader(c, WithBufferSize(8<<10))
						if err != nil {
							t.Errorf("server NewBlockingReader: %v", err)
							return
						}
						// Pause server read briefly to test on-turn non-blocking overflow protection!
						srvReaderCh <- r
					},
				},
			}
			stopSrv := startBlockingEvents(t, srvEv)
			defer stopSrv()
			ln, err := srvEv.Listen("tcp://127.0.0.1:0", opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()

			cliEv := &Events{
				Pollers:             1,
				AllowHalfOpen:       true,
				BlockingWrite:       true,
				MaxOutboundBuffered: 8 << 10,
				Handler: FuncHandler{
					Read: func(c Conn) error {
						// On-turn overflow MUST return ErrOutboundOverflow immediately, NEVER deadlock!
						_, _ = c.Discard(-1)
						_, werr := c.Write(make([]byte, 32<<10))
						select {
						case turnOverflowCh <- werr:
						default:
						}
						return nil
					},
				},
			}
			stopCli := startBlockingEvents(t, cliEv)
			defer stopCli()

			cli, err := cliEv.Dial("tcp://"+ln.Addr().String(), dialOpts...)
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			srvR := <-srvReaderCh

			// Business goroutines call cli.Write / cli.Writev / cli.WriteOwned directly with oversized
			// payloads + concurrent small writers (anti-starvation FIFO check)!
			bigChunk := bytes.Repeat([]byte("BIG-FRAME-"), 2500) // 25 KiB (> 8 KiB MaxOutboundBuffered!)
			vecPart1 := bytes.Repeat([]byte("VEC1-"), 1200)      // 6 KiB
			vecPart2 := bytes.Repeat([]byte("VEC2-"), 1200)      // 6 KiB
			ownedPayload := bytes.Repeat([]byte("OWND-"), 2400)  // 12 KiB
			smallFrame := bytes.Repeat([]byte("s"), 128)
			const smallWorkers = 6
			const smallIters = 20

			var wg sync.WaitGroup
			errCh := make(chan error, 16)

			wg.Add(1)
			go func() {
				defer wg.Done()
				if n, err := cli.Write(bigChunk); err != nil || n != len(bigChunk) {
					errCh <- err
					return
				}
				if n, err := cli.Writev([][]byte{vecPart1, vecPart2}); err != nil || n != len(vecPart1)+len(vecPart2) {
					errCh <- err
					return
				}
				owned := AcquireBuffer(len(ownedPayload))
				_, _ = owned.Write(ownedPayload)
				if n, err := cli.WriteOwned(owned); err != nil || n != len(ownedPayload) {
					errCh <- err
					return
				}
			}()

			for wIdx := range smallWorkers {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					for range smallIters {
						var n int
						var err error
						switch idx % 3 {
						case 0:
							n, err = cli.Write(smallFrame)
						case 1:
							n, err = cli.Writev([][]byte{smallFrame[:64], smallFrame[64:]})
						default:
							ob := AcquireBuffer(len(smallFrame))
							_, _ = ob.Write(smallFrame)
							n, err = cli.WriteOwned(ob)
						}
						if err != nil || n != len(smallFrame) {
							errCh <- err
							return
						}
					}
				}(wIdx)
			}

			go func() {
				wg.Wait()
				_ = cli.CloseWrite()
			}()

			wantTotal := len(bigChunk) + len(vecPart1) + len(vecPart2) + len(ownedPayload) + smallWorkers*smallIters*len(smallFrame)
			received, err := io.ReadAll(srvR)
			if err != nil {
				t.Fatalf("srvR ReadAll = %v", err)
			}
			select {
			case werr := <-errCh:
				t.Fatalf("concurrent blocking Conn.Write failed: %v", werr)
			default:
			}
			if len(received) != wantTotal {
				t.Fatalf("received %d bytes, want %d", len(received), wantTotal)
			}
			// Verify bigChunk was written whole without smallFrame barging into the middle of it!
			if !bytes.Contains(received, bigChunk) {
				t.Fatal("bigChunk was torn/interleaved by concurrent small writers")
			}

			// Verify SetWriteDeadline wakes a stalled BlockingWrite Conn.Write!
			silentLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer silentLn.Close()
			acceptedRaw := make(chan net.Conn, 1)
			go func() {
				c, _ := silentLn.Accept()
				if c != nil {
					acceptedRaw <- c
				}
			}()
			stalledCli, err := cliEv.Dial("tcp://"+silentLn.Addr().String(), dialOpts...)
			if err != nil {
				t.Fatal(err)
			}
			rawPeer := <-acceptedRaw
			defer rawPeer.Close()
			// Trigger OnRead on stalledCli to verify on-turn overflow returns ErrOutboundOverflow!
			_, _ = rawPeer.Write([]byte("ping"))
			if onTurnErr := <-turnOverflowCh; !errors.Is(onTurnErr, ErrOutboundOverflow) {
				t.Fatalf("on-turn Write overflow = %v, want ErrOutboundOverflow", onTurnErr)
			}

			_ = stalledCli.SetWriteDeadline(time.Now().Add(40 * time.Millisecond))
			_, werr := stalledCli.Write(make([]byte, 8<<20)) // exceeds kernel + user buffers -> blocks -> deadline closes!
			if werr == nil {
				t.Fatal("expected stalled BlockingWrite to fail on SetWriteDeadline expiry")
			}
		})
	}
}

type shortTestWriter struct{ max int }

func (s shortTestWriter) Write(p []byte) (int, error) {
	if s.max < 0 {
		return -1, io.ErrShortWrite
	}
	if len(p) > s.max {
		return s.max, nil
	}
	return len(p), nil
}

func TestBlockingAttachToClosingConnAndLayerWriteToContracts(t *testing.T) {
	readDone := make(chan struct{})
	srvEv := &Events{
		Pollers:       1,
		AllowHalfOpen: true,
		Handler: FuncHandler{
			Read: func(c Conn) error {
				if c.InboundBuffered() == 0 {
					return nil
				}
				if n, err := c.WriteTo(shortTestWriter{max: -1}); n != 0 || err == nil {
					t.Errorf("negative layerNode.WriteTo = %d, %v", n, err)
				}
				if n, err := c.WriteTo(shortTestWriter{max: 2}); err != io.ErrShortWrite || n != 2 {
					t.Errorf("short layerNode.WriteTo = %d, %v", n, err)
				}
				var rest bytes.Buffer
				if _, err := c.WriteTo(&rest); err != nil {
					t.Errorf("full layerNode.WriteTo = %v", err)
				}
				if n, err := c.WriteTo(io.Discard); n != 0 || err != nil {
					t.Errorf("empty layerNode.WriteTo = %d, %v", n, err)
				}
				close(readDone)
				return nil
			},
		},
	}
	stopSrv := startBlockingEvents(t, srvEv)
	defer stopSrv()
	ln, err := srvEv.Listen("tcp://127.0.0.1:0", WithLayers(PassThroughLayer))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cliEv := &Events{Pollers: 1, AllowHalfOpen: true}
	stopCli := startBlockingEvents(t, cliEv)
	defer stopCli()
	cli, err := cliEv.Dial("tcp://"+ln.Addr().String(), WithLayers(PassThroughLayer))
	if err != nil {
		t.Fatal(err)
	}

	bw, _ := NewBlockingWriter(cli)
	_, _ = bw.WriteOwned(nil)
	ob := AcquireBuffer(8)
	_, _ = ob.WriteString("hello")
	_, _ = bw.WriteOwned(ob)
	<-readDone

	_ = cli.Close()
	// Attaching NewBlockingConn after local Close() on both layered & plain conns returns net.ErrClosed immediately!
	bc, err := NewBlockingConn(cli)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Read(make([]byte, 8)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Read on pre-closed layered BlockingConn = %v, want net.ErrClosed", err)
	}
	if _, err := bc.Writer().WriteOwned(func() *Buffer { b := AcquireBuffer(4); _, _ = b.WriteString("x"); return b }()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("WriteOwned on pre-closed layered BlockingConn = %v", err)
	}

	plainCli, cleanupPlain := runBlockingPair(t, false, true, 0, 0, nil, func(c Conn) { _ = c.CloseWrite() })
	defer cleanupPlain()
	_ = plainCli.Close()
	plainBC0, err := NewBlockingConn(plainCli)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = plainBC0.Read(make([]byte, 8))

	plainCli2, cleanupPlain2 := runBlockingPair(t, false, true, 0, 0, nil, func(c Conn) { _ = c.CloseWrite() })
	defer cleanupPlain2()
	_ = plainCli2.Abort(errors.New("custom abort"))
	plainBC, err := NewBlockingConn(plainCli2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plainBC.Read(make([]byte, 8)); err == nil {
		t.Fatal("Read on aborted plain BlockingConn succeeded")
	}

	// Exercise writeOwnedPreservePartial with nil, overflow, and partial-overflow layer!
	var partialLayer LayerFunc = func(LayerContext) LayerHandler { return partialOverflowLayer{} }
	pCli, cleanupPartial := runBlockingPair(t, false, true, 0, 0, nil, nil)
	defer cleanupPartial()
	_ = pCli
	partialEv := &Events{Pollers: 1, Handler: FuncHandler{}}
	_ = partialEv.initConfig()
	tStack := contractStack(&contractTransport{}, FuncHandler{}, partialLayer.Attach(nil))
	topNode := tStack.layers[0]
	if n, err := topNode.writeOwnedPreservePartial(nil); n != 0 || err != nil {
		t.Fatalf("nil writeOwnedPreservePartial = %d, %v", n, err)
	}
	pBuf := AcquireBuffer(16)
	_, _ = pBuf.WriteString("0123456789")
	if n, err := topNode.writeOwnedPreservePartial(pBuf); n != 4 || !errors.Is(err, ErrOutboundOverflow) || pBuf.Len() != 6 {
		t.Fatalf("partial writeOwnedPreservePartial = %d, %v, rem=%d", n, err, pBuf.Len())
	}
	// Remaining 3 bytes fit <= 4 and consume pBuf!
	pBuf.Discard(3)
	if n, err := topNode.writeOwnedPreservePartial(pBuf); n != 3 || err != nil {
		t.Fatalf("fit writeOwnedPreservePartial = %d, %v", n, err)
	}
	// Closed stack writeOwnedPreservePartial releases buffer and returns ErrClosed!
	tStack.closed(nil)
	tStack.closed(nil) // idempotent markClosed false branch
	pBuf2 := AcquireBuffer(8)
	_, _ = pBuf2.WriteString("xy")
	if _, err := topNode.writeOwnedPreservePartial(pBuf2); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed writeOwnedPreservePartial = %v", err)
	}

	// dialProbe.fail(nil) defaults to net.ErrClosed and stack.closed(nil) forwards closeCause!
	dp := &dialProbe{done: make(chan struct{})}
	dp.fail(nil)
	if !errors.Is(dp.err, net.ErrClosed) {
		t.Fatalf("dialProbe.fail(nil) err = %v", dp.err)
	}
	unopenedStack := &stack{
		socket: &contractTransport{state: StateReadEnded},
		top:    FuncHandler{},
		layers: []*layerNode{{conn: passThroughHandler{}}},
	}
	unopenedStack.layers[0].stack = unopenedStack
	dp2 := &dialProbe{done: make(chan struct{})}
	unopenedStack.probe.Store(dp2)
	unopenedStack.recordCloseCause(io.ErrUnexpectedEOF)
	if err := unopenedStack.endOfStream(false); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unopened endOfStream = %v, want ErrUnexpectedEOF", err)
	}
	unopenedStack.closed(nil)
	if !errors.Is(dp2.err, io.ErrUnexpectedEOF) {
		t.Fatalf("closed probe err = %v, want ErrUnexpectedEOF", dp2.err)
	}

	// Layered Close() followed immediately by Abort() cuts drain and notifies blockio.State!
	cutStack := contractStack(&contractTransport{}, FuncHandler{}, passThroughHandler{})
	cutTop := cutStack.layers[0]
	_, _ = NewBlockingWriter(cutTop)
	_ = cutTop.CloseWrite()
	_ = cutTop.Close()
	_ = cutTop.Abort(errors.New("cut drain"))

	// Attaching blockio.State to a freshly closed stack (c.block == nil && c.over()) + reserveRefused isolation!
	overStack := contractStack(&contractTransport{}, FuncHandler{}, passThroughHandler{})
	overStack.limit = 16
	overStack.enableWriteWatch()
	_ = overStack.reserve(10)
	if err := overStack.reserve(10); !errors.Is(err, ErrOutboundOverflow) || !overStack.reserveRefused.Load() {
		t.Fatalf("concurrent reserve overflow = %v, reserveRefused=%v", err, overStack.reserveRefused.Load())
	}
	overStack.release(10)
	if overStack.reserveRefused.Load() {
		t.Fatal("release did not clear reserveRefused")
	}
	overStack.recordCloseCause(errors.New("already over"))
	overStack.closed(errors.New("already over"))
	if _, ok := (*layerNode)(nil).connBlockState(); ok {
		t.Fatal("nil layerNode returned block state")
	}
	if _, ok := (blockControlGetter{}).GetBlockState("invalid"); ok {
		t.Fatal("non-Conn returned block state")
	}
	if _, err := NewBlockingConn(overStack.layers[0]); err != nil {
		t.Fatal(err)
	}
	sbs := stackBlockSource{top: overStack.layers[0]}
	_ = sbs.MaxInbound()
	_ = sbs.Wake()
	_ = sbs.IsOverflow(ErrOutboundOverflow)
}

type partialOverflowLayer struct{ PassThrough }

func (partialOverflowLayer) OnWrite(_ LayerContext, p []byte) (int, error) {
	if len(p) > 4 {
		return 4, ErrOutboundOverflow
	}
	return len(p), nil
}
