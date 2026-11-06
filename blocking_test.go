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
	cliEv := &Events{
		Pollers:             1,
		AllowHalfOpen:       halfOpen,
		MaxInboundBuffered:  maxIn,
		MaxOutboundBuffered: maxOut,
		Handler:             FuncHandler{},
	}
	stopCliEv := startBlockingEvents(t, cliEv)
	cli, err := cliEv.Adopt(rawNet, adoptOpts...)
	if err != nil {
		_ = ln.Close()
		stopCliEv()
		stopEv()
		t.Fatal(err)
	}

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
