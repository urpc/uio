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

// Command holepunch demonstrates establishing a KCP+TLS session between two UDP
// peers from their bound ports — the hole-punching flow that uio's layered
// PacketListener.DialPeer enables.
//
// Run it from the examples directory:
//
//	go run ./holepunch
//
// The whole demo runs in one process on loopback:
//
//	rendezvous (TCP) ─── swaps the two peers' UDP addresses
//
//	peer A (dial, client chain)            peer B (accept, server chain)
//	  WriteTo(punch) ──────────────────►     pre-creates B's session peer
//	  DialPeerContext(B) ── ClientHello ─►   B's stack handshakes
//	  ◄───────────────────── ServerHello     punches back; Handler.Open fires
//	  ═══════════ encrypted KCP stream ═══════════►
//	  Write(msg) ──────────────────────►     echo
//	  ◄────────────────────────── echo ──    Read collects it
//
// The point DialPeer makes: everything A sends leaves from A's *bound* UDP
// port, the same port whose public mapping a STUN probe would discover — so
// the handshake itself punches the hole, and ukcp's retransmission carries it
// through the NAT's initial drops. B never calls DialPeer: an incoming peer
// builds its stack on the first datagram and Handler.Open hands it the session.
//
// On loopback there is no NAT, so only the dialer sends raw punches (they
// pre-create the acceptor's session peer); the acceptor's handshake response
// punches back. Behind a symmetric NAT both sides send raw punches first, and
// the dialer then finds the peer already present — ErrPeerExists — and uses the
// connection that call returns. In a real deployment the address a peer
// registers is its STUN-discovered public mapping (not its bind address, as
// here) and the rendezvous runs over the network — the DialPeer flow is
// unchanged. Roles are agreed over the rendezvous: the dialer's listener
// carries a client chain, the acceptor's a server chain, because TLS has a
// fixed client/server role (KCP itself is symmetric).
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ukcp"
	"github.com/urpc/uio/utls"
)

func main() {
	if err := runDemo(); err != nil {
		fmt.Println("holepunch:", err)
		os.Exit(1)
	}
}

func runDemo() error {
	// A rendezvous peers register with to learn each other's UDP address. Here
	// it is a loopback TCP listener; in production it is a network server.
	sigLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer sigLn.Close()
	go serveSignaling(sigLn)
	fmt.Println("rendezvous on tcp://" + sigLn.Addr().String())

	serverTLS, clientTLS := demoTLS()
	sigAddr := sigLn.Addr().String()
	errCh := make(chan error, 2)
	go func() { errCh <- runPeer("accept", sigAddr, serverTLS, clientTLS) }()
	go func() { errCh <- runPeer("dial", sigAddr, serverTLS, clientTLS) }()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			return err
		}
	}
	fmt.Println("hole-punch demo complete: KCP+TLS session established from the bound ports")
	return nil
}

// runPeer binds a layered UDP listener, registers with the rendezvous, punches
// toward the peer, and then dials (role "dial") or accepts (role "accept") a
// KCP+TLS session and echoes a message across it.
func runPeer(role, sigAddr string, serverTLS, clientTLS *tls.Config) error {
	var chain []uio.Layer
	if role == "dial" {
		chain = []uio.Layer{
			ukcp.ClientLayer(ukcp.Options{}),
			utls.ClientLayer(utls.LayerOptions{TLSConfig: clientTLS}),
		}
	} else {
		chain = []uio.Layer{
			ukcp.ServerLayer(ukcp.Options{}),
			utls.ServerLayer(utls.LayerOptions{TLSConfig: serverTLS}),
		}
	}

	msg := []byte("hello across the punched hole")
	var (
		started   = make(chan struct{})
		mu        sync.Mutex
		got       []byte // dial: the echo collected
		doneOnce  sync.Once
		done      = make(chan struct{}) // closed once the full echo arrived
		closeOnce sync.Once
		closed    = make(chan struct{}) // closed once the session ended
		session   = make(chan uio.Conn, 1)
	)
	events := &uio.Events{
		Pollers: 1,
		Hooks: uio.FuncHandler{
			Start: func(*uio.Events) { close(started) },
		},
		Handler: uio.FuncHandler{
			Open: func(c uio.Conn) {
				if role == "accept" {
					fmt.Println("accept: session opened from", c.RemoteAddr())
					session <- c
				}
			},
			Read: func(c uio.Conn) error {
				if role == "accept" {
					_, err := c.WriteTo(c) // echo the request back
					return err
				}
				n := c.InboundBuffered()
				peeked := c.Peek(make([]byte, n))
				mu.Lock()
				got = append(got, peeked...)
				full := len(got) >= len(msg)
				mu.Unlock()
				_, _ = c.Discard(len(peeked))
				if full {
					doneOnce.Do(func() { close(done) })
				}
				return nil
			},
			Close: func(uio.Conn, error) {
				closeOnce.Do(func() { close(closed) })
			},
		},
	}
	go events.Serve()
	defer events.Close(nil)
	<-started // Listen is allowed only once Serve is ready

	ln, err := events.Listen("udp://127.0.0.1:0", uio.WithLayers(chain...))
	if err != nil {
		return err
	}
	packets := ln.(uio.PacketListener)
	local := ln.Addr().(*net.UDPAddr).AddrPort()
	fmt.Printf("%s: bound udp://%s\n", role, local)

	// Register the bound address and learn the peer's. A real peer registers
	// its STUN-discovered public mapping here instead of its bind address.
	peerAddr, err := exchange(sigAddr, role, local)
	if err != nil {
		return err
	}
	fmt.Printf("%s: peer is udp://%s\n", role, peerAddr)

	// Punch from the bound port toward the peer. The dialer's raw punches open
	// its NAT mapping and pre-create the acceptor's session peer; the handshake
	// below then completes it, and the acceptor's response punches back. On
	// loopback there is no NAT, so this is illustrative. Behind a symmetric NAT
	// both sides punch first, and the dialer finds the peer already present
	// (ErrPeerExists) and uses the connection that call returns.
	if role == "dial" {
		for i := 0; i < 5; i++ {
			if _, err := packets.WriteTo([]byte("punch"), peerAddr); err != nil {
				return err
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	if role == "dial" {
		// DialPeer is synchronous and waits for a turn to open the stack, so
		// it runs on this goroutine — never inside a callback. ctx bounds the
		// handshake wait.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := packets.DialPeerContext(ctx, peerAddr, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("dial: %w", err)
		}
		fmt.Println("dial: session opened to", conn.RemoteAddr())
		if _, err := conn.Write(msg); err != nil {
			return err
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("dial: echo timed out")
		}
		mu.Lock()
		echo := append([]byte(nil), got...)
		mu.Unlock()
		if !bytes.Equal(echo, msg) {
			return fmt.Errorf("dial: echo mismatch: %q", echo)
		}
		fmt.Printf("dial: echoed %q\n", echo)
		// Close gracefully and wait for it to complete, so the close frame
		// reaches the acceptor before this peer's Events tears down.
		_ = conn.Close()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("dial: close did not complete")
		}
		return nil
	}

	// accept: the incoming session arrives through Handler.Open; Handler.Read
	// echoes the request back. Finish when the dialer closes the session.
	select {
	case <-session:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("accept: no session arrived")
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("accept: session did not close")
	}
	fmt.Println("accept: echoed the request back, session closed")
	return nil
}

// exchange registers role and local with the rendezvous and returns the peer's
// UDP address: one line out ("role addr"), one line back (the peer's addr).
func exchange(sigAddr, role string, local netip.AddrPort) (netip.AddrPort, error) {
	conn, err := net.Dial("tcp", sigAddr)
	if err != nil {
		return netip.AddrPort{}, err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s %s\n", role, local); err != nil {
		return netip.AddrPort{}, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.ParseAddrPort(strings.TrimSpace(line))
}

// serveSignaling accepts the two peers, reads each one's registration, and
// sends each the other's UDP address. A production rendezvous also authenticates
// the peers and carries the temporary keys a roaming session would bind to.
func serveSignaling(ln net.Listener) {
	type reg struct {
		conn net.Conn
		addr netip.AddrPort
	}
	regs := make([]reg, 0, 2)
	for i := 0; i < 2; i++ {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			conn.Close()
			return
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			conn.Close()
			return
		}
		addr, err := netip.ParseAddrPort(fields[1])
		if err != nil {
			conn.Close()
			return
		}
		regs = append(regs, reg{conn: conn, addr: addr})
	}
	for i, r := range regs {
		fmt.Fprintf(r.conn, "%s\n", regs[1-i].addr)
		r.conn.Close()
	}
}

// demoTLS builds a self-signed certificate for 127.0.0.1 and a client config
// that trusts it. A real deployment uses proper certificates.
func demoTLS() (server, client *tls.Config) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "holepunch-demo"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	server = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS13,
	}
	client = &tls.Config{
		RootCAs:    pool,
		ServerName: "127.0.0.1",
		MinVersion: tls.VersionTLS13,
	}
	return server, client
}
