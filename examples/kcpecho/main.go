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

// Command kcpecho runs a quiet KCP-over-UDP echo server for external load testing.
//
// Run it from the examples directory:
//
//	go run ./kcpecho -addr :9528
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ukcp"
)

func main() {
	addr := flag.String("addr", ":9528", "UDP listen address")
	pollers := flag.Int("pollers", 0, "event loop count; zero uses the uio default")
	buffer := flag.Int("buffer", 2048, "maximum bytes read per socket read")
	sockBuf := flag.Int("sockbuf", 4<<20, "kernel UDP socket read/write buffer size in bytes")
	mtu := flag.Int("mtu", 1400, "KCP maximum transmission unit (24..1500)")
	sndWnd := flag.Int("sndwnd", 1024, "KCP send window size in segments")
	rcvWnd := flag.Int("rcvwnd", 1024, "KCP receive window size in segments")
	noDelay := flag.Bool("nodelay", true, "enable KCP NoDelay (30ms min RTO)")
	interval := flag.Duration("interval", 10*time.Millisecond, "KCP internal flush interval")
	resend := flag.Int("resend", 2, "KCP fast retransmit duplicate ACK threshold (-1 disables)")
	cc := flag.Bool("cc", false, "enable KCP congestion window")
	lockOS := flag.Bool("lockos", false, "lock each event loop to its OS thread")
	reusePort := flag.Bool("reuseport", true, "set SO_REUSEPORT to scale UDP queues across CPU cores")
	flag.Parse()

	listenAddr := *addr
	if !strings.Contains(listenAddr, "://") {
		listenAddr = "udp://" + listenAddr
	}

	kcpOpts := ukcp.Options{
		MTU:               *mtu,
		SendWindow:        *sndWnd,
		RecvWindow:        *rcvWnd,
		NoDelay:           *noDelay,
		Interval:          *interval,
		Resend:            *resend,
		CongestionControl: *cc,
	}
	if err := kcpOpts.Validate(); err != nil {
		fmt.Println("invalid KCP options:", err)
		os.Exit(1)
	}

	started := make(chan struct{})
	events := &uio.Events{
		Pollers:        *pollers,
		ReadBufferSize: *buffer,
		LockOSThread:   *lockOS,
		ReusePort:      *reusePort,
		Hooks: uio.FuncHandler{
			Start: func(*uio.Events) { close(started) },
		},
		Handler: uio.FuncHandler{
			Read: func(c uio.Conn) error {
				_, err := c.WriteTo(c)
				return err
			},
		},
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- events.Serve()
	}()
	<-started

	listenOpts := []uio.ListenOption{uio.WithLayers(ukcp.ServerLayer(kcpOpts))}
	if *sockBuf > 0 {
		listenOpts = append(listenOpts, uio.WithListenControl(func(_, _ string, fd uintptr) error {
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, *sockBuf)
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, *sockBuf)
			return nil
		}))
	}
	ln, err := events.Listen(listenAddr, listenOpts...)
	if err != nil {
		_ = events.Close(err)
		fmt.Println("listen failed:", err)
		os.Exit(1)
	}
	defer ln.Close()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		_ = events.Close(nil)
	}()

	fmt.Printf("kcpecho listening on udp://%s (mtu=%d sndwnd=%d rcvwnd=%d nodelay=%v cc=%v reuseport=%v)\n",
		ln.Addr().String(), *mtu, *sndWnd, *rcvWnd, *noDelay, *cc, *reusePort)
	if err := <-serveErr; err != nil {
		fmt.Println("server exited with error:", err)
		os.Exit(1)
	}
}
