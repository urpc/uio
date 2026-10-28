// Package utls runs TLS as a protocol layer over a UIO transport, with
// crypto/tls doing the cryptography.
//
// Server and Client return a [uio.Layer]: stack one with [uio.Events.Use]
// and it hands everything above it plaintext, so whatever runs there — a
// protocol handler, another layer, another TLS layer — speaks TLS without
// knowing it:
//
//	events.Use(utls.Server(config))
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
// The handshake is bounded by the layer's own timer, whose expiry closes the
// connection. A connection opens
// to the handler above once its handshake settles; one accepted whose
// handshake fails or times out reaches OnClose instead, with its cause, and a
// dial that fails before it settles reports its cause from DialContext, the
// handler hearing nothing of it. Close sends the peer
// close_notify once what was accepted has left, and CloseWrite sends it one
// way; a peer's close_notify delivers what arrived with it and then the
// close. The connection below's deadlines and fd-shaped options reach
// through to it.
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
// each costing a syscall. On UIO that is [uio.Events.MaxBufferSize], whose
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

// errHandshakeTimeout ends a connection whose handshake outlasted its bound.
var errHandshakeTimeout = fmt.Errorf("utls: handshake timeout: %w", os.ErrDeadlineExceeded)

// tlsLayer is what Server and Client build: the configuration its
// connections run with, and the bounds it keeps.
type tlsLayer struct {
	config           *Config
	client           bool
	handshakeTimeout time.Duration
	stagingBound     int
}

// Option configures the layer Server or Client builds.
type Option func(*tlsLayer)

// HandshakeTimeout bounds the handshake. Zero means
// DefaultHandshakeTimeout, a negative value means no bound.
func HandshakeTimeout(timeout time.Duration) Option {
	return func(m *tlsLayer) { m.handshakeTimeout = timeout }
}

// StagingBound bounds the decrypted bytes the layer holds for the handler
// above. Once it holds its bound it stops consuming the connection below, so a
// pause at the top stills the wire. During the handshake it bounds the
// ciphertext the layer takes from below that the handshake has not read yet,
// though never below one TLS record. Zero means DefaultStagingBound.
func StagingBound(bytes int) Option {
	return func(m *tlsLayer) { m.stagingBound = bytes }
}

// Server serves the layer above over TLS with config: certificates, ALPN,
// whatever a crypto/tls server runs with.
func Server(config *Config, opts ...Option) uio.Layer {
	m := &tlsLayer{config: config}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Client takes the client side of TLS with config, for a dial: config needs
// ServerName, or InsecureSkipVerify, as it does for crypto/tls.Client.
func Client(config *Config, opts ...Option) uio.Layer {
	m := &tlsLayer{config: config, client: true}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Attach starts TLS on one connection.
func (m *tlsLayer) Attach(ctx uio.LayerContext) uio.LayerHandler {
	return &conn{tlsLayer: m, ctx: ctx}
}
