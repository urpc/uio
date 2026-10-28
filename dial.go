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
	"net"
	"strings"
	"syscall"

	"github.com/libp2p/go-reuseport"
)

// SocketControl runs on a socket's descriptor, for the options the dedicated
// setters do not carry: IP_TTL for a NAT probe, IP_TOS, SO_MARK, or an option
// a hole punch needs before its socket connects or binds. network and address
// are what the socket is for; fd is valid only for the call.
type SocketControl func(network, addr string, fd uintptr) error

// DialConfig is what a dial's options collected into; see DialOption.
type DialConfig struct {
	// LocalAddr binds the dial's socket to this local address before it
	// connects, in the address syntax of the network dialed: "127.0.0.1:0",
	// "[::1]:0", "unix:///tmp/app.rnd". A second dial from the same address
	// fails unless the option set on the socket allows the sharing.
	LocalAddr string

	// Control runs on the dialing socket's descriptor, after the socket is
	// created and before LocalAddr binds it or it connects — as net.Dialer's
	// Control does, so a bind-time option such as SO_REUSEPORT can be set.
	Control SocketControl

	// layers is this connection's own chain; only WithLayers sets it.
	// hasLayers tells an override, of one layer or none, from no override.
	layers    []Layer
	hasLayers bool
}

// DialOption configures one dial; see WithLocalAddr and WithDialControl.
type DialOption func(*DialConfig)

// WithLocalAddr binds the dial's socket to addr; see DialConfig.LocalAddr.
func WithLocalAddr(addr string) DialOption {
	return func(config *DialConfig) { config.LocalAddr = addr }
}

// WithLayers runs this dial or adoption with its own chain instead of the
// Events' Use chain — the layers given, in install order, the first closest
// to the wire, for this connection alone. It is how one client serves several
// schemes: a wss:// dial brings its TLS layer, a ws:// dial
// WithLayers(PassThroughLayer) — or nothing, which stands for the same.
//
// The Events must have layers installed — its Serve resolves the stack that
// runs them — and a packet connection carries none: both are refused before
// the socket moves. Adopt and AdoptContext honor it; the other options shape
// a dial's socket and do not apply to an adoption.
func WithLayers(layers ...Layer) DialOption {
	return func(config *DialConfig) {
		config.layers = append([]Layer(nil), layers...)
		if len(config.layers) == 0 {
			// A stack always has a layer: an override of none is the layer
			// that changes nothing.
			config.layers = []Layer{PassThroughLayer}
		}
		config.hasLayers = true
	}
}

// WithDialControl runs control on the dialing socket before it connects; see
// DialConfig.Control.
func WithDialControl(control SocketControl) DialOption {
	return func(config *DialConfig) { config.Control = control }
}

// ListenConfig is what a Listen's options collected into; see ListenOption.
type ListenConfig struct {
	// Control runs on the listening socket's descriptor before it binds.
	// With ReusePort it runs after the sharing option itself was set.
	Control SocketControl
}

// ListenOption configures one Listen; see WithListenControl.
type ListenOption func(*ListenConfig)

// WithListenControl runs control on the listening socket before it binds; see
// ListenConfig.Control.
func WithListenControl(control SocketControl) ListenOption {
	return func(config *ListenConfig) { config.Control = control }
}

// dialConfigFor collects opts.
func dialConfigFor(opts []DialOption) DialConfig {
	var config DialConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&config)
		}
	}
	return config
}

// listenConfigFor collects opts.
func listenConfigFor(opts []ListenOption) ListenConfig {
	var config ListenConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&config)
		}
	}
	return config
}

// netDialerFor builds the net.Dialer of one dial.
func netDialerFor(network string, config DialConfig) (*net.Dialer, error) {
	dialer := &net.Dialer{}
	if config.LocalAddr != "" {
		switch {
		case strings.HasPrefix(network, "tcp"):
			addr, err := net.ResolveTCPAddr(network, config.LocalAddr)
			if err != nil {
				return nil, err
			}
			dialer.LocalAddr = addr
		case strings.HasPrefix(network, "udp"):
			addr, err := net.ResolveUDPAddr(network, config.LocalAddr)
			if err != nil {
				return nil, err
			}
			dialer.LocalAddr = addr
		case strings.HasPrefix(network, "unix"):
			// Accept the scheme-prefixed form, as the dial target does.
			name := config.LocalAddr
			if rest, ok := strings.CutPrefix(name, network+"://"); ok {
				name = rest
			}
			dialer.LocalAddr = &net.UnixAddr{Name: name, Net: network}
		default:
			return nil, ErrUnsupported
		}
	}
	if config.Control != nil {
		dialer.Control = rawControl(config.Control)
	}
	return dialer, nil
}

// netListenConfigFor builds the net.ListenConfig of one listener: the
// sharing option of ReusePort first, then the application's control.
func netListenConfigFor(reusePort bool, control SocketControl) net.ListenConfig {
	var config net.ListenConfig
	switch {
	case reusePort && control != nil:
		config.Control = func(network, address string, conn syscall.RawConn) error {
			if err := reuseport.Control(network, address, conn); err != nil {
				return err
			}
			return rawControl(control)(network, address, conn)
		}
	case reusePort:
		config.Control = reuseport.Control
	case control != nil:
		config.Control = rawControl(control)
	}
	return config
}

// rawControl adapts a SocketControl to a net control callback: the descriptor
// is reached through the raw connection, which also keeps the socket from
// closing under the call.
func rawControl(control SocketControl) func(network, address string, conn syscall.RawConn) error {
	return func(network, address string, conn syscall.RawConn) error {
		var err error
		if cerr := conn.Control(func(fd uintptr) { err = control(network, address, fd) }); cerr != nil {
			return cerr
		}
		return err
	}
}
