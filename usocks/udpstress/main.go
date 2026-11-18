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

// Command udpstress drives concurrent SOCKS5 UDP ASSOCIATE sessions through an
// in-process usocks proxy to a UDP echo target, reporting datagram throughput
// and resource usage (FD/goroutines/RSS) plus a post-close cleanup check.
//
// Run on Linux (reads /proc):
//
//	go run ./usocks/udpstress -n 1000 -duration 10s -msg 512
package main

import (
	"encoding/binary"
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
	"time"

	"github.com/urpc/uio/usocks"
)

func main() {
	n := flag.Int("n", 1000, "concurrent UDP associations")
	duration := flag.Duration("duration", 10*time.Second, "traffic duration after setup")
	msgSize := flag.Int("msg", 512, "datagram payload size")
	ramp := flag.Duration("ramp", 2*time.Second, "stagger setup over this window")
	flag.Parse()

	target := udpEchoTarget()
	srv := usocks.NewServer(usocks.Options{})
	go srv.Events().Serve()
	defer func() { _ = srv.Close(); _ = srv.Wait() }()
	proxyAddr := listenReady(srv)
	fmt.Printf("udp target %s  proxy %s  n=%d msg=%d duration=%s\n", target, proxyAddr, *n, *msgSize, *duration)

	baseGoroutines := runtime.NumGoroutine()
	baseFD := fdCount()
	var ops, setupDone, failed atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	start := time.Now()

	perConn := time.Duration(0)
	if *n > 0 && *ramp > 0 {
		perConn = *ramp / time.Duration(*n)
	}
	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := udpSession(proxyAddr, target, *msgSize, stop, &ops, &setupDone); err != nil {
				failed.Add(1)
				setupDone.Add(1)
			}
		}()
		if perConn > 0 {
			time.Sleep(perConn)
		}
	}
	for setupDone.Load() < uint64(*n) {
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Printf("setup: %d associations in %v, failed=%d\n", setupDone.Load(), time.Since(start).Round(time.Millisecond), failed.Load())

	opsBase := ops.Load()
	time.Sleep(*duration)
	close(stop)
	wg.Wait()
	total := ops.Load() - opsBase
	fmt.Printf("steady: %d datagrams in %s = %.0f dgram/s\n", total, *duration, float64(total)/duration.Seconds())
	fmt.Printf("peak-ish: fd=%d goroutines=%d rss=%s\n", fdCount(), runtime.NumGoroutine(), rss())

	_ = srv.Close()
	_ = srv.Wait()
	time.Sleep(2 * time.Second)
	runtime.GC()
	time.Sleep(500 * time.Millisecond)
	fmt.Printf("post-close: goroutines=%d (base %d, delta %+d) fd=%d (base %d, delta %+d)\n",
		runtime.NumGoroutine(), baseGoroutines, runtime.NumGoroutine()-baseGoroutines, fdCount(), baseFD, fdCount()-baseFD)
}

func udpEchoTarget() string {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		panic(err)
	}
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, addr, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteToUDP(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().String()
}

func listenReady(srv *usocks.Server) string {
	deadline := time.Now().Add(5 * time.Second)
	for {
		ln, err := srv.Listen("tcp://127.0.0.1:0")
		if err == nil {
			return ln.Addr().String()
		}
		if time.Now().After(deadline) {
			panic(err)
		}
		time.Sleep(time.Millisecond)
	}
}

func udpSession(proxyAddr, target string, msgSize int, stop <-chan struct{}, ops, setupDone *atomic.Uint64) error {
	tcp, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		return err
	}
	defer tcp.Close()
	_ = tcp.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := tcp.Write([]byte{5, 1, 0}); err != nil {
		return err
	}
	if _, err := io.ReadFull(tcp, make([]byte, 2)); err != nil {
		return err
	}
	if _, err := tcp.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(tcp, head); err != nil {
		return err
	}
	if head[1] != 0 {
		return fmt.Errorf("rep %d", head[1])
	}
	ip := net.IP(readN(tcp, 4))
	port := binary.BigEndian.Uint16(readN(tcp, 2))
	bnd := &net.UDPAddr{IP: ip, Port: int(port)}
	_ = tcp.SetDeadline(time.Time{})
	setupDone.Add(1) // association established; traffic begins

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return err
	}
	defer udp.Close()

	tip := net.ParseIP(strings.Split(target, ":")[0]).To4()
	tport, _ := strconv.Atoi(strings.Split(target, ":")[1])
	payload := make([]byte, msgSize)
	frame := make([]byte, 0, 10+msgSize)
	frame = append(frame, 0, 0, 0, 1)
	frame = append(frame, tip...)
	frame = binary.BigEndian.AppendUint16(frame, uint16(tport))
	frame = append(frame, payload...)
	reply := make([]byte, 64<<10)
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		if _, err := udp.WriteToUDP(frame, bnd); err != nil {
			return nil
		}
		_ = udp.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, _, err := udp.ReadFromUDP(reply); err != nil {
			return nil
		}
		ops.Add(1)
	}
}

func readN(c net.Conn, n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		panic(err)
	}
	return b
}

func fdCount() int {
	e, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(e)
}

func rss() string {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "?"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				kb, _ := strconv.ParseUint(f[1], 10, 64)
				return fmt.Sprintf("%.1fMB", float64(kb)/1024)
			}
		}
	}
	return "?"
}
