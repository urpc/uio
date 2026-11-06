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
	"context"
	"net"
	"net/url"
	"strings"
	"sync"
	"syscall"

	"github.com/libp2p/go-reuseport"
)

// SocketControl runs on a socket's descriptor, for the options the dedicated
// setters do not carry: IP_TTL for a NAT probe, IP_TOS, SO_MARK, or an option
// a hole punch needs before its socket connects or binds. network and address
// are what the socket is for; fd is valid only for the call.
type SocketControl func(network, addr string, fd uintptr) error

// dialConfig is what a dial's options collected into; see DialOption.
type dialConfig struct {
	// localAddr, control and userdata are WithLocalAddr's, WithDialControl's
	// and WithUserdata's; the option funcs carry their documentation, as they
	// are what a caller names.
	localAddr string
	control   SocketControl
	userdata  any

	// layers is this endpoint's chain; only WithLayers sets it. hasLayers
	// tells a chain, of one layer or none, from none. A Dialer's own config
	// is copied per dial, so an applier writes the copy: it replaces the
	// slice whole and never appends to or writes into its array, which a
	// Dialer's concurrent dials share.
	layers    []Layer
	hasLayers bool
}

// listenConfig is what a listen's options collected into; see ListenOption.
type listenConfig struct {
	// control and userdata are WithListenControl's and WithUserdata's.
	control  SocketControl
	userdata any

	// layers is this listener's chain; see dialConfig.layers.
	layers    []Layer
	hasLayers bool
}

// adoptConfig is what an adoption's options collected into; see AdoptOption.
type adoptConfig struct {
	// userdata is WithUserdata's.
	userdata any

	// layers is the adopted connection's chain; see dialConfig.layers.
	layers    []Layer
	hasLayers bool
}

// DialOption configures one dial: the defaults of a Dialer, or one Dialer
// call's own. A Listen takes ListenOption, an adoption AdoptOption; WithLayers
// and WithUserdata are all three, the options a dial, a listen and an
// adoption share.
type DialOption interface{ applyDial(*dialConfig) }

// ListenOption configures one listener; see Listen.
type ListenOption interface{ applyListen(*listenConfig) }

// AdoptOption configures one adoption; see Adopt.
type AdoptOption interface{ applyAdopt(*adoptConfig) }

// layersOption is the chain an endpoint runs: WithLayers' value. It is a
// DialOption, a ListenOption and an AdoptOption, so the same call reads the
// same everywhere.
type layersOption []Layer

// WithLayers runs this endpoint with its own chain instead of none — the
// layers given, in install order, the first closest to the wire. A Listen
// gives its listener the chain; a dial, or one Dialer call, gives that
// connection. On a Dialer call it replaces the Dialer's own chain for that
// call. WithLayers() with no layers runs the layer that changes nothing
// (PassThroughLayer), so a chain always has a layer.
func WithLayers(layers ...Layer) layersOption {
	normalized := append([]Layer(nil), layers...)
	if len(normalized) == 0 {
		normalized = []Layer{PassThroughLayer}
	}
	return layersOption(normalized)
}

func (o layersOption) applyDial(c *dialConfig) {
	c.layers, c.hasLayers = []Layer(o), true
}

func (o layersOption) applyListen(c *listenConfig) {
	c.layers, c.hasLayers = []Layer(o), true
}

func (o layersOption) applyAdopt(c *adoptConfig) {
	c.layers, c.hasLayers = []Layer(o), true
}

// userdataOption is WithUserdata's value.
type userdataOption struct{ value any }

// WithUserdata sets the connection's initial Userdata: for a dial or an
// adoption, that one connection's; for a Listen, every connection it accepts.
// A Dialer's default is each of its dials', and a dial's own WithUserdata
// overrides it.
func WithUserdata(value any) userdataOption { return userdataOption{value: value} }

func (o userdataOption) applyDial(c *dialConfig)     { c.userdata = o.value }
func (o userdataOption) applyListen(c *listenConfig) { c.userdata = o.value }
func (o userdataOption) applyAdopt(c *adoptConfig)   { c.userdata = o.value }

// localAddrOption is WithLocalAddr's value.
type localAddrOption string

// WithLocalAddr binds the dial's socket to addr before it connects, in the
// address syntax of the network dialed: "127.0.0.1:0", "[::1]:0",
// "unix:///tmp/app.rnd". A second dial from the same address fails unless
// the option set on the socket allows the sharing.
func WithLocalAddr(addr string) localAddrOption { return localAddrOption(addr) }

func (o localAddrOption) applyDial(c *dialConfig) { c.localAddr = string(o) }

// dialControlOption is WithDialControl's value.
type dialControlOption SocketControl

// WithDialControl runs control on the dialing socket's descriptor, after the
// socket is created and before WithLocalAddr binds it or it connects — as
// net.Dialer's Control does, so a bind-time option such as SO_REUSEPORT can
// be set.
func WithDialControl(control SocketControl) dialControlOption {
	return dialControlOption(control)
}

func (o dialControlOption) applyDial(c *dialConfig) { c.control = SocketControl(o) }

// listenControlOption is WithListenControl's value.
type listenControlOption SocketControl

// WithListenControl runs control on the listening socket's descriptor before
// it binds. With ReusePort it runs after the sharing option itself was set.
func WithListenControl(control SocketControl) listenControlOption {
	return listenControlOption(control)
}

func (o listenControlOption) applyListen(c *listenConfig) { c.control = SocketControl(o) }

// dialConfigFor collects opts.
func dialConfigFor(opts []DialOption) dialConfig {
	var config dialConfig
	for _, opt := range opts {
		if opt != nil {
			opt.applyDial(&config)
		}
	}
	return config
}

// listenConfigFor collects opts.
func listenConfigFor(opts []ListenOption) listenConfig {
	var config listenConfig
	for _, opt := range opts {
		if opt != nil {
			opt.applyListen(&config)
		}
	}
	return config
}

// adoptConfigFor collects opts.
func adoptConfigFor(opts []AdoptOption) adoptConfig {
	var config adoptConfig
	for _, opt := range opts {
		if opt != nil {
			opt.applyAdopt(&config)
		}
	}
	return config
}

// netDialerFor builds the net.Dialer of one dial.
func netDialerFor(network string, config dialConfig) (*net.Dialer, error) {
	dialer := &net.Dialer{}
	if config.localAddr != "" {
		switch {
		case strings.HasPrefix(network, "tcp"):
			addr, err := net.ResolveTCPAddr(network, config.localAddr)
			if err != nil {
				return nil, err
			}
			dialer.LocalAddr = addr
		case strings.HasPrefix(network, "udp"):
			addr, err := net.ResolveUDPAddr(network, config.localAddr)
			if err != nil {
				return nil, err
			}
			dialer.LocalAddr = addr
		case strings.HasPrefix(network, "unix"):
			// Accept the scheme-prefixed form, as the dial target does.
			name := config.localAddr
			if rest, ok := strings.CutPrefix(name, network+"://"); ok {
				name = rest
			}
			dialer.LocalAddr = &net.UnixAddr{Name: name, Net: network}
		default:
			return nil, ErrUnsupported
		}
	}
	if config.control != nil {
		dialer.Control = rawControl(config.control)
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

// Dial connects to the address on the named network.
//
// Known networks are "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6", "ip",
// "ip4", "ip6", "unix", "unixgram" and "unixpacket".
func (ev *Events) Dial(addr string, opts ...DialOption) (Conn, error) {
	return ev.defaultDialer().Dial(addr, opts...)
}

// DialContext connects synchronously and allows cancellation while resolving
// or establishing the connection. Never call it from an event loop goroutine.
// A connection it returns gets OnOpen and, at its end, OnClose, even if it is
// closed first; after an error no callback runs for it.
func (ev *Events) DialContext(dialCtx context.Context, addr string, opts ...DialOption) (Conn, error) {
	return ev.defaultDialer().DialContext(dialCtx, addr, opts...)
}

// Adopt transfers ownership of an established stream connection to ev. The
// caller must not use conn after calling Adopt, including when Adopt returns an
// error. ev must already be serving.
func (ev *Events) Adopt(conn net.Conn, opts ...AdoptOption) (Conn, error) {
	return ev.AdoptContext(context.Background(), conn, opts...)
}

// AdoptContext is Adopt with ctx bounding the wait for any installed layers to
// open.
func (ev *Events) AdoptContext(ctx context.Context, conn net.Conn, opts ...AdoptOption) (Conn, error) {
	if conn == nil {
		return nil, ErrUnsupported
	}
	if !ev.ready.Load() || ev.closing.Load() {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	cfg := adoptConfigFor(opts)
	if cfg.hasLayers && isEventLoopGoroutine() {
		_ = conn.Close()
		return nil, ErrDialOnEventLoop
	}
	if err := refuseLayerPacket(conn.LocalAddr().Network(), cfg.layers, cfg.hasLayers); err != nil {
		_ = conn.Close()
		return nil, err
	}
	entry, err := ev.entryFor(cfg.layers, cfg.hasLayers)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	fdc, err := ev.newAdoptedConn(conn)
	if err != nil {
		return nil, err
	}
	return ev.finishEndpointConn(ctx, fdc, entry, cfg.userdata)
}

// Dialer dials through one Events with private defaults — the options it was
// built with, a chain among them — which each Dial's own options override.
type Dialer struct {
	ev  *Events
	cfg dialConfig

	entryOnce sync.Once
	entry     *entry
	entryErr  error
}

// Dialer returns a Dialer of ev with opts as its defaults.
func (ev *Events) Dialer(opts ...DialOption) (*Dialer, error) {
	cfg := dialConfigFor(opts)
	if err := validateChain(cfg.layers, cfg.hasLayers); err != nil {
		return nil, err
	}
	return &Dialer{ev: ev, cfg: cfg}, nil
}

func (ev *Events) defaultDialer() *Dialer { return &Dialer{ev: ev} }

func (d *Dialer) dialerEntry() (*entry, error) {
	if !d.cfg.hasLayers {
		return d.ev.plainEntry(), nil
	}
	d.entryOnce.Do(func() { d.entry, d.entryErr = d.ev.newEntry(d.cfg.layers) })
	return d.entry, d.entryErr
}

// Dial dials addr with this Dialer; see DialContext.
func (d *Dialer) Dial(addr string, opts ...DialOption) (Conn, error) {
	return d.DialContext(context.Background(), addr, opts...)
}

// DialContext connects synchronously through this Dialer's Events.
func (d *Dialer) DialContext(dialCtx context.Context, addr string, opts ...DialOption) (Conn, error) {
	ev := d.ev
	config := d.cfg
	callLayers := false
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if _, ok := opt.(layersOption); ok {
			callLayers = true
		}
		opt.applyDial(&config)
	}
	if !ev.ready.Load() || ev.closing.Load() {
		return nil, net.ErrClosed
	}
	if isEventLoopGoroutine() {
		return nil, ErrDialOnEventLoop
	}
	if !strings.Contains(addr, "://") {
		addr = "tcp://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return nil, err
	}
	if err := refuseLayerPacket(u.Scheme, config.layers, config.hasLayers); err != nil {
		return nil, err
	}
	var entry *entry
	if callLayers {
		entry, err = ev.newEntry(config.layers)
	} else {
		entry, err = d.dialerEntry()
	}
	if err != nil {
		return nil, err
	}
	address := u.Host
	if strings.HasPrefix(u.Scheme, "unix") {
		address = u.Path
	}
	dialer, err := netDialerFor(u.Scheme, config)
	if err != nil {
		return nil, err
	}
	conn, err := dialer.DialContext(dialCtx, u.Scheme, address)
	if err != nil {
		return nil, err
	}
	fdc, err := ev.newDialedConn(u.Scheme, conn)
	if err != nil {
		return nil, err
	}
	return ev.finishEndpointConn(dialCtx, fdc, entry, config.userdata)
}

func (ev *Events) finishEndpointConn(ctx context.Context, fdc *fdConn, entry *entry, userdata any) (Conn, error) {
	fdc.entry = entry
	fdc.SetUserdata(userdata)
	var waitDial func(context.Context) (Conn, error)
	if entry.dial != nil {
		waitDial = entry.dial(fdc)
	}
	fdc.loop = ev.selectLoop()
	if err := ev.addConnContext(ctx, fdc); err != nil {
		return nil, err
	}
	if waitDial == nil {
		return fdc, nil
	}
	return waitDial(ctx)
}
