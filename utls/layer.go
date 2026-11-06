// Package utls runs TLS as a protocol layer over a UIO transport, with
// crypto/tls doing the cryptography.
//
// ServerLayer and ClientLayer return a [uio.Layer]: name one in a Listen's or a
// Dial's [uio.WithLayers] and it hands everything above it plaintext, so
// whatever runs there — a protocol handler, another layer, another TLS
// layer — speaks TLS without knowing it:
//
//	listener, err := events.Listen(addr, uio.WithLayers(
//		utls.ServerLayer(utls.LayerOptions{TLSConfig: serverConfig})))
//	conn, err := events.Dial(addr, uio.WithLayers(
//		utls.ClientLayer(utls.LayerOptions{TLSConfig: clientConfig})))
//
// Nothing costs a goroutine. The handshake runs as a coroutine that OnRead
// resumes with the bytes that arrive: when crypto/tls asks its transport
// for more than the round carried, the transport hands control back and the
// next round goes on from there, so the rounds the executor already
// serializes carry the handshake between them. The executor's goroutines
// passing the rounds on to each other must not be locked to OS threads.
// Records after the handshake are decrypted in OnRead, straight from the
// bytes the round read; a record that arrives across several rounds stays
// cut in crypto/tls's buffer until it is whole.
//
// A record carries up to 16 KiB, so a transport whose reads are smaller
// carries every record through several rounds: set Events.ReadBufferSize to
// at least a record. 16 KiB reads — the cadence crypto/tls runs at — move a
// bulk transfer about 1.4× the 4 KiB default's, ahead of crypto/tls on the
// same wire; 64 KiB reads measured 1.4–1.8× the default and fastest on both
// hosts we ran.
//
// The handshake is bounded by the layer's own timer, whose expiry closes the
// connection. A connection opens
// to the handler above once its handshake settles; a connection whose
// handshake fails or times out — accepted or dialed — ends without a handler
// callback (the Handler never opened, so it hears neither OnOpen nor
// OnClose), and a dial reports its cause from DialContext. Close sends the
// peer close_notify once what was accepted has left, and CloseWrite sends it
// one way, then shuts the connection below's writing half — a stack below
// that cannot end one direction reports ErrUnsupported after close_notify
// went. A peer's close_notify delivers what arrived with it and then the
// close. The connection below's deadlines pass through directly, and its
// socket options are reached through [uio.SocketOf].
//
// One crypto/tls behavior this relies on is not a written contract: that a
// temporary net.Error from the transport Read makes it keep the partial
// record it buffered and retry. The tests pin it, so a Go release that
// changes it fails here rather than in the field.
//
// Session tickets are the Config's, as they are for crypto/tls: servers that
// share one Config resume one another's sessions, and servers that each build
// their own do not — the client falls back to a full handshake, which is
// slower, not an error. Share one Config across every Events a process serves
// through, and install the same ticket keys with
// [tls.Config.SetSessionTicketKeys] on every process that shares an address.
//
// Size the transport's reads to the record: a TLS record carries up to 16 KiB,
// and a socket read smaller than that splits one record across several rounds,
// each costing a syscall. On UIO that is [uio.Events.ReadBufferSize], whose
// default of 4 KiB turns one 16 KiB record into four reads; a transfer reads
// about twice as fast with 16 KiB and about half again as fast with 64 KiB.
package utls

import (
	stdtls "crypto/tls"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/urpc/uio"
)

// Config is the TLS configuration the layer runs with: certificates on a
// server, ServerName or InsecureSkipVerify on a client, ALPN either way. It
// is crypto/tls's own configuration, so anything crypto/tls accepts here
// works, and a Config built for crypto/tls needs no conversion.
type Config = stdtls.Config

// ConnectionState is what a settled connection's State reports.
type ConnectionState = stdtls.ConnectionState

// DefaultHandshakeTimeout bounds a handshake when the layer is installed
// without the HandshakeTimeout option.
const DefaultHandshakeTimeout = 10 * time.Second

// DefaultStagingBound is the decrypted-byte bound the layer is installed
// with when it is installed without the StagingBound option.
const DefaultStagingBound = 64 << 10

// readBufferSize is the largest record crypto/tls hands back from one Read;
// a drain never splits a record's plaintext across calls.
const readBufferSize = 16 << 10

// handshakeTakeFloor is the least the handshake takes from below at once,
// whatever StagingBound says: one whole record, ciphertext and header, so a
// tiny bound slows nothing to a byte at a time.
const handshakeTakeFloor = readBufferSize + 1<<10

// errWouldBlock is what the transport tells crypto/tls when a read would
// have to wait for bytes the connection below has not delivered yet.
var errWouldBlock error = wouldBlockError{}

type wouldBlockError struct{}

func (wouldBlockError) Error() string   { return "utls: tls read would block" }
func (wouldBlockError) Timeout() bool   { return false }
func (wouldBlockError) Temporary() bool { return true }

var errClosed = errors.New("utls: connection closed")

// ErrInvalidOptions reports options no connection through the layer can run
// with; every one of them fails with it. LayerOptions.Validate says so up
// front.
var ErrInvalidOptions = errors.New("utls: invalid options")

// errHandshakeTimeout ends a connection whose handshake outlasted its bound.
var errHandshakeTimeout = fmt.Errorf("utls: handshake timeout: %w", os.ErrDeadlineExceeded)

// tlsLayer is what ServerLayer and ClientLayer build: the configuration its
// connections run with, the bounds it keeps, and the options' fault, which
// fails every connection through it.
type tlsLayer struct {
	config           *Config
	client           bool
	handshakeTimeout time.Duration
	stagingBound     int
	err              error
}

// LayerOptions configures the layer ServerLayer or ClientLayer builds.
type LayerOptions struct {
	// TLSConfig is what the layer's connections run with: certificates and
	// ALPN on a server; ServerName or InsecureSkipVerify on a client, as for
	// crypto/tls.Client. It is required.
	TLSConfig *Config
	// HandshakeTimeout bounds the handshake. Zero means
	// DefaultHandshakeTimeout, a negative value means no bound.
	HandshakeTimeout time.Duration
	// StagingBound bounds the decrypted bytes the layer holds for the handler
	// above: at its bound the layer stops taking new chunks from the
	// connection below, so a pause at the top stills the wire. It paces the
	// rounds rather than capping one delivery: a chunk in hand is decrypted
	// whole — stopping mid-chunk would carry its ciphertext through a copy
	// into the next round — and crypto/tls hands a record over whole
	// however its bytes arrived, one it had been assembling from earlier
	// rounds included, so the delivery may pass the bound by that record (up
	// to 16 KiB) and the turn's chunk (one socket read). The framework
	// pauses the layer at its own delivery bound (DefaultDeliverBound, or
	// MaxInboundBuffered) as well, so a larger StagingBound is capped there.
	// During the handshake it bounds the ciphertext the layer takes from
	// below that the handshake has not read yet, though never below one TLS
	// record. Zero means DefaultStagingBound.
	StagingBound int
}

// Validate reports what a layer built from o would refuse: a missing
// TLSConfig, today.
func (o LayerOptions) Validate() error {
	if o.TLSConfig == nil {
		return fmt.Errorf("%w: TLSConfig is required", ErrInvalidOptions)
	}
	return nil
}

// newLayer builds the layer's policy. Options it refuses fail every
// connection through the layer with the cause, as uws and ukcp layers do.
func newLayer(opts LayerOptions, client bool) uio.Layer {
	m := &tlsLayer{
		config:           opts.TLSConfig,
		client:           client,
		handshakeTimeout: opts.HandshakeTimeout,
		stagingBound:     opts.StagingBound,
	}
	m.err = opts.Validate()
	return m
}

// ServerLayer runs the server side of TLS as a [uio.Layer]: certificates,
// ALPN, whatever a crypto/tls server runs with. Options it refuses fail
// every connection through it with ErrInvalidOptions.
func ServerLayer(opts LayerOptions) uio.Layer {
	return newLayer(opts, false)
}

// ClientLayer runs the client side of TLS as a [uio.Layer], for a dial: it
// needs ServerName, or InsecureSkipVerify, as crypto/tls.Client does. One
// layer dials one connection at a time; its options are shared by every
// connection through it.
func ClientLayer(opts LayerOptions) uio.Layer {
	return newLayer(opts, true)
}

// Attach starts TLS on one connection.
func (m *tlsLayer) Attach(ctx uio.LayerContext) uio.LayerHandler {
	return &conn{tlsLayer: m, ctx: ctx}
}
