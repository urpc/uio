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

// Command loadtest drives concurrent SOCKS5 CONNECT sessions through an
// in-process usocks proxy to an in-process echo target, and reports scale
// metrics: setup time, throughput, latency, peak RSS/FD/goroutines, and a
// post-close leak check. It is a measurement tool, not a server.
//
// Run on Linux (reads /proc for RSS/FD):
//
//	go run ./usocks/loadtest -n 10000 -duration 10s
//	go run ./usocks/loadtest -n 100000 -duration 20s -msg 256
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ubridge"
	"github.com/urpc/uio/usocks"
)

func main() {
	n := flag.Int("n", 10000, "concurrent sessions")
	duration := flag.Duration("duration", 10*time.Second, "traffic duration after setup")
	msgSize := flag.Int("msg", 64, "echo message size in bytes")
	ramp := flag.Duration("ramp", 2*time.Second, "stagger session setup over this window")
	deadline := flag.Duration("deadline", 30*time.Second, "per-session setup deadline (applies to dial too)")
	srcips := flag.Int("srcips", 1, "source loopback IPs (127.0.0.1..N) to multiply ephemeral ports")
	plain := flag.Bool("plain", false, "use a plain ubridge tunnel (no SOCKS) for splice comparison")
	flag.Parse()

	targetAddr, targetEv := startTarget()
	srv, proxyAddr := startProxy(*plain, targetAddr)
	fmt.Printf("target %s  proxy %s  n=%d msg=%d duration=%s\n", targetAddr, proxyAddr, *n, *msgSize, *duration)

	baseGoroutines := runtime.NumGoroutine()
	baseFD := fdCount()

	var ops atomic.Uint64
	var setupDone atomic.Uint64
	var failed atomic.Uint64
	var dialFail atomic.Uint64
	var handshakeFail atomic.Uint64
	var trafficFail atomic.Uint64
	var portExhausted atomic.Uint64
	lat := newLatency()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	start := time.Now()

	// Sample peak resource usage while the load runs.
	peak := newPeak()
	sampleStop := make(chan struct{})
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-sampleStop:
				return
			case <-t.C:
				peak.sample()
			}
		}
	}()

	perConnDelay := time.Duration(0)
	if *n > 0 && *ramp > 0 {
		perConnDelay = *ramp / time.Duration(*n)
	}
	for i := 0; i < *n; i++ {
		wg.Add(1)
		localIP := fmt.Sprintf("127.0.0.%d", 1+i%*srcips)
		go func() {
			defer wg.Done()
			if err := runSession(proxyAddr, targetAddr, localIP, *msgSize, *deadline, *plain, stop, &ops, &setupDone, &dialFail, &handshakeFail, &trafficFail, &portExhausted, lat); err != nil {
				failed.Add(1)
				setupDone.Add(1) // count failed setups so the setup wait completes
			}
		}()
		if perConnDelay > 0 {
			time.Sleep(perConnDelay)
		}
	}

	// Wait for setup to finish, then measure steady-state traffic: reset the
	// counters so ops/s and latency exclude the setup window.
	for setupDone.Load() < uint64(*n) {
		time.Sleep(10 * time.Millisecond)
	}
	setupTime := time.Since(start)
	fmt.Printf("setup: %d sessions in %v (%.0f/s), failed=%d (dial=%d handshake=%d port-exhausted=%d)\n",
		setupDone.Load(), setupTime.Round(time.Millisecond), float64(*n)/setupTime.Seconds(), failed.Load(), dialFail.Load(), handshakeFail.Load(), portExhausted.Load())
	if s := dialErrSample.Load(); s != nil {
		fmt.Printf("first dial error: %s\n", *s)
	}

	opsBaseline := ops.Load()
	lat.reset()
	time.Sleep(*duration)
	close(stop)
	wg.Wait()
	close(sampleStop)
	peak.sample()

	elapsed := *duration
	totalOps := ops.Load() - opsBaseline
	p50, p99 := lat.percentiles()
	fmt.Printf("steady traffic: %d ops in %s = %.0f ops/s, traffic-errors=%d\n", totalOps, elapsed, float64(totalOps)/elapsed.Seconds(), trafficFail.Load())
	fmt.Printf("latency: p50=%s p99=%s\n", p50.Round(time.Microsecond), p99.Round(time.Microsecond))
	fmt.Printf("peak: rss=%s fd=%d goroutines=%d\n", peak.rss(), peak.fd, peak.goroutines)
	fmt.Printf("proxy stats: %s\n", srv.stats())

	// Leak check: after every session closed and both servers stopped,
	// resources should fall back toward baseline (GC may lag, so allow a
	// settling window).
	_ = srv.ev.Close(nil)
	_ = srv.ev.Wait()
	_ = targetEv.Close(nil)
	_ = targetEv.Wait()
	time.Sleep(2 * time.Second)
	runtime.GC()
	time.Sleep(500 * time.Millisecond)
	postGoroutines := runtime.NumGoroutine()
	postFD := fdCount()
	fmt.Printf("post-close: goroutines=%d (baseline %d, delta %+d), fd=%d (baseline %d, delta %+d), rss=%s\n",
		postGoroutines, baseGoroutines, postGoroutines-baseGoroutines, postFD, baseFD, postFD-baseFD, rssString())
	if postGoroutines > baseGoroutines+50 {
		fmt.Printf("LEAK SUSPECT: goroutines did not return to baseline\n")
	}
}

func startTarget() (string, *uio.Events) {
	started := make(chan struct{})
	ev := &uio.Events{
		Handler: uio.FuncHandler{Read: func(c uio.Conn) error {
			_, err := c.WriteTo(c)
			return err
		}},
		Hooks: uio.FuncHandler{Start: func(*uio.Events) { close(started) }},
	}
	go ev.Serve()
	<-started
	ln, err := ev.Listen("tcp://127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	return ln.Addr().String(), ev
}

type proxyHandle struct {
	ev    *uio.Events
	stats func() string
}

func startProxy(plain bool, target string) (proxyHandle, string) {
	var ev *uio.Events
	var stats func() string
	var listen func(string) (uio.Listener, error)
	if plain {
		br := &ubridge.Bridge{Upstream: target}
		ev = br.Events()
		stats = func() string { return fmt.Sprintf("%+v", br.Stats()) }
		listen = func(a string) (uio.Listener, error) { return ev.Listen(a) }
	} else {
		s := usocks.NewServer(usocks.Options{})
		ev = s.Events()
		stats = func() string { return fmt.Sprintf("%+v", s.Stats()) }
		listen = s.Listen
	}
	go ev.Serve()
	var ln uio.Listener
	var err error
	for i := 0; i < 500; i++ {
		if ln, err = listen("tcp://127.0.0.1:0"); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		panic(err)
	}
	return proxyHandle{ev: ev, stats: stats}, ln.Addr().String()
}

var dialErrSample atomic.Pointer[string]

// runSession establishes one SOCKS5 session and echoes until stop.
func runSession(proxyAddr, targetAddr, localIP string, msgSize int, deadline time.Duration, plain bool, stop <-chan struct{}, ops, setupDone, dialFail, handshakeFail, trafficFail, portExhausted *atomic.Uint64, lat *latency) error {
	dialer := net.Dialer{
		Timeout:   deadline,
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(localIP)},
	}
	c, err := dialer.Dial("tcp", proxyAddr)
	if err != nil {
		dialFail.Add(1)
		if errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, syscall.EADDRNOTAVAIL) {
			portExhausted.Add(1) // ephemeral source ports exhausted (loopback single-IP cap)
		}
		s := err.Error()
		dialErrSample.CompareAndSwap(nil, &s)
		return err
	}
	defer c.Close()
	if !plain {
		host, portStr, _ := net.SplitHostPort(targetAddr)
		port, _ := strconv.Atoi(portStr)
		if err := socksConnect(c, host, uint16(port), deadline); err != nil {
			handshakeFail.Add(1)
			return err
		}
	}
	setupDone.Add(1) // session established; traffic begins
	msg := make([]byte, msgSize)
	echo := make([]byte, msgSize)
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		t0 := time.Now()
		if _, err := c.Write(msg); err != nil {
			trafficFail.Add(1)
			return nil
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second)) // bound a wedged peer
		if _, err := io.ReadFull(c, echo); err != nil {
			trafficFail.Add(1)
			return nil
		}
		lat.record(time.Since(t0))
		ops.Add(1)
	}
}

func socksConnect(c net.Conn, host string, port uint16, deadline time.Duration) error {
	_ = c.SetDeadline(time.Now().Add(deadline))
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}
	m := make([]byte, 2)
	if _, err := io.ReadFull(c, m); err != nil {
		return err
	}
	if m[1] != 0x00 {
		return fmt.Errorf("method %d", m[1])
	}
	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, net.ParseIP(host).To4()...)
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := c.Write(req); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		return err
	}
	if head[1] != 0x00 {
		return fmt.Errorf("rep %d", head[1])
	}
	bnd := make([]byte, 6)
	if _, err := io.ReadFull(c, bnd); err != nil {
		return err
	}
	_ = c.SetDeadline(time.Time{})
	return nil
}

// latency records a histogram of round-trip times at 1ms resolution, covering
// 0–4.096s so high percentiles are not censored at a low ceiling.
type latency struct {
	mu      sync.Mutex
	buckets [4096]int // 1ms each; overflow in the last bucket
	count   uint64
}

func newLatency() *latency { return &latency{} }

func (l *latency) reset() {
	l.mu.Lock()
	l.buckets = [4096]int{}
	l.count = 0
	l.mu.Unlock()
}

func (l *latency) record(d time.Duration) {
	b := d.Milliseconds()
	if b < 0 {
		b = 0
	}
	if b >= 4096 {
		b = 4095
	}
	l.mu.Lock()
	l.buckets[b]++
	l.count++
	l.mu.Unlock()
}

func (l *latency) percentiles() (p50, p99 time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count == 0 {
		return 0, 0
	}
	return l.at(0.50), l.at(0.99)
}

func (l *latency) at(q float64) time.Duration {
	target := uint64(float64(l.count) * q)
	var cum uint64
	for b, c := range l.buckets {
		cum += uint64(c)
		if cum >= target {
			return time.Duration(b) * time.Millisecond
		}
	}
	return 4096 * time.Millisecond
}

type peak struct {
	mu         sync.Mutex
	rssBytes   uint64
	fd         int
	goroutines int
}

func newPeak() *peak { return &peak{} }

func (p *peak) sample() {
	r := rssBytes()
	f := fdCount()
	g := runtime.NumGoroutine()
	p.mu.Lock()
	if r > p.rssBytes {
		p.rssBytes = r
	}
	if f > p.fd {
		p.fd = f
	}
	if g > p.goroutines {
		p.goroutines = g
	}
	p.mu.Unlock()
}

func (p *peak) rss() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return humanBytes(p.rssBytes)
}

func fdCount() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

func rssBytes() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseUint(fields[1], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

func rssString() string { return humanBytes(rssBytes()) }

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}
