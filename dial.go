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

	// layers is this endpoint's chain; only WithLayers sets it. hasLayers
	// tells a chain, of one layer or none, from none.
	layers    []Layer
	hasLayers bool
}

// ListenConfig is what a listen's options collected into; see ListenOption.
type ListenConfig struct {
	// Control runs on the listening socket's descriptor before it binds.
	// With ReusePort it runs after the sharing option itself was set.
	Control SocketControl

	// layers is this listener's chain; see DialConfig.layers.
	layers    []Layer
	hasLayers bool
}

// AdoptConfig is what an adoption's options collected into; see AdoptOption.
type AdoptConfig struct {
	// layers is the adopted connection's chain; see DialConfig.layers.
	layers    []Layer
	hasLayers bool
}

// DialOption configures one dial: the defaults of a Dialer, or one Dialer
// call's own. A Listen takes ListenOption, an adoption AdoptOption; WithLayers
// is all three, the one option that names a chain.
type DialOption interface{ applyDial(*DialConfig) }

// ListenOption configures one listener; see Listen.
type ListenOption interface{ applyListen(*ListenConfig) }

// AdoptOption configures one adoption; see Adopt.
type AdoptOption interface{ applyAdopt(*AdoptConfig) }

// LayersOption is the chain an endpoint runs: WithLayers' value. It is a
// DialOption, a ListenOption and an AdoptOption, so the same call reads the
// same everywhere.
type LayersOption []Layer

// WithLayers runs this endpoint with its own chain instead of none — the
// layers given, in install order, the first closest to the wire. A Listen
// gives its listener the chain; a dial, or one Dialer call, gives that
// connection. On a Dialer call it replaces the Dialer's own chain for that
// call. WithLayers() with no layers runs the layer that changes nothing
// (PassThroughLayer), so a chain always has a layer.
func WithLayers(layers ...Layer) LayersOption {
	normalized := append([]Layer(nil), layers...)
	if len(normalized) == 0 {
		normalized = []Layer{PassThroughLayer}
	}
	return LayersOption(normalized)
}

func (o LayersOption) applyDial(c *DialConfig) {
	c.layers, c.hasLayers = []Layer(o), true
}

func (o LayersOption) applyListen(c *ListenConfig) {
	c.layers, c.hasLayers = []Layer(o), true
}

func (o LayersOption) applyAdopt(c *AdoptConfig) {
	c.layers, c.hasLayers = []Layer(o), true
}

// localAddrOption is WithLocalAddr's value.
type localAddrOption string

// WithLocalAddr binds the dial's socket to addr; see DialConfig.LocalAddr.
func WithLocalAddr(addr string) localAddrOption { return localAddrOption(addr) }

func (o localAddrOption) applyDial(c *DialConfig) { c.LocalAddr = string(o) }

// dialControlOption is WithDialControl's value.
type dialControlOption SocketControl

// WithDialControl runs control on the dialing socket before it connects; see
// DialConfig.Control.
func WithDialControl(control SocketControl) dialControlOption {
	return dialControlOption(control)
}

func (o dialControlOption) applyDial(c *DialConfig) { c.Control = SocketControl(o) }

// listenControlOption is WithListenControl's value.
type listenControlOption SocketControl

// WithListenControl runs control on the listening socket before it binds; see
// ListenConfig.Control.
func WithListenControl(control SocketControl) listenControlOption {
	return listenControlOption(control)
}

func (o listenControlOption) applyListen(c *ListenConfig) { c.Control = SocketControl(o) }

// dialConfigFor collects opts.
func dialConfigFor(opts []DialOption) DialConfig {
	var config DialConfig
	for _, opt := range opts {
		if opt != nil {
			opt.applyDial(&config)
		}
	}
	return config
}

// listenConfigFor collects opts.
func listenConfigFor(opts []ListenOption) ListenConfig {
	var config ListenConfig
	for _, opt := range opts {
		if opt != nil {
			opt.applyListen(&config)
		}
	}
	return config
}

// adoptConfigFor collects opts.
func adoptConfigFor(opts []AdoptOption) AdoptConfig {
	var config AdoptConfig
	for _, opt := range opts {
		if opt != nil {
			opt.applyAdopt(&config)
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
