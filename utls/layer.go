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

// Package utls runs TLS as a layer over a UIO transport, with crypto/tls
// doing the cryptography.
//
// A Layer sits in a stack, as ulayer serves them, and hands the layer above
// plaintext: the connection it wraps reads decrypted and writes encrypted,
// so whatever runs above it — a protocol handler, another stack, another TLS
// layer — speaks TLS without knowing it:
//
//	transport := ulayer.Over(events, handler, utls.Server(config))
//
// Nothing costs a goroutine. The handshake runs as a coroutine that OnData
// resumes with the bytes that arrive: when crypto/tls asks its transport
// for more than the round carried, the transport hands control back and the
// next round goes on from there, so the rounds the executor already
// serializes carry the handshake between them. The executor's goroutines
// passing the rounds on to each other must not be locked to OS threads.
// Records after the handshake are decrypted in OnData, straight from the
// bytes the round read; a record that arrives across several rounds stays
// cut in crypto/tls's buffer until it is whole.
//
// The handshake is bounded by a deadline the layer sets on the connection
// below, whose expiry closes it, as UIO's deadlines do. A connection opens
// to the layer above once its handshake settles; one whose handshake fails
// or times out reaches OnClose instead, with its cause. Close sends the peer
// close_notify once what was accepted has left, and CloseWrite sends it one
// way; a peer's close_notify delivers what arrived with it and then the
// close. The connection below's deadlines and fd-shaped options reach
// through to it, as ulayer.Forward carries them.
//
// One crypto/tls behavior this relies on is not a written contract: that a
// temporary net.Error from the transport Read makes it keep the partial
// record it buffered and retry. The tests pin it, so a Go release that
// changes it fails here rather than in the field.
package utls

import (
	stdtls "crypto/tls"
	"errors"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ulayer"
)

// DefaultHandshakeTimeout bounds a handshake when Layer leaves
// HandshakeTimeout at zero.
const DefaultHandshakeTimeout = 10 * time.Second

// DefaultStagingBound is the decrypted-byte bound Layer leaves StagingBound
// at zero.
const DefaultStagingBound = 64 << 10

// readBufferSize is the largest record crypto/tls hands back from one Read;
// a drain never splits a record's plaintext across calls.
const readBufferSize = 16 << 10

// errWouldBlock is what the transport tells crypto/tls when a read would
// have to wait for bytes the connection below has not delivered yet.
var errWouldBlock error = wouldBlockError{}

type wouldBlockError struct{}

func (wouldBlockError) Error() string   { return "utls: tls read would block" }
func (wouldBlockError) Timeout() bool   { return false }
func (wouldBlockError) Temporary() bool { return true }

var errClosed = errors.New("utls: connection closed")

// Layer runs TLS on the connections it serves and hands the layer above
// plaintext. Configure it before the first connection; the crypto/tls Config
// is the one the handshakes run with, not copied.
type Layer struct {
	// Config is the crypto/tls configuration: certificates on a server,
	// ServerName or InsecureSkipVerify on a client, ALPN either way.
	Config *stdtls.Config
	// Client takes the client side of the handshake, as Client sets it;
	// a server leaves it false.
	Client bool
	// HandshakeTimeout bounds the handshake. Zero means
	// DefaultHandshakeTimeout, a negative value means no bound.
	HandshakeTimeout time.Duration
	// StagingBound bounds the decrypted bytes the layer holds for the layer
	// above. Once it holds its bound the layer stops consuming the
	// connection below, so a pause at the top stills the wire, as ulayer's
	// contract says. Zero means DefaultStagingBound.
	StagingBound int
}

// Server returns a layer that serves TLS with config.
func Server(config *stdtls.Config) *Layer {
	return &Layer{Config: config}
}

// Client returns a layer that takes the client side of TLS with config,
// for a dial: config needs ServerName, or InsecureSkipVerify, as it does for
// crypto/tls.Client.
func Client(config *stdtls.Config) *Layer {
	return &Layer{Config: config, Client: true}
}

// Handler implements ulayer.Layer.
func (l *Layer) Handler(next uio.Handler) uio.Handler {
	perConn := ulayer.Func(func(lower uio.Conn, next uio.Handler) uio.Handler {
		c := &conn{
			Forward: ulayer.Forward{Lower: lower},
			layer:   l,
			next:    next,
		}
		return uio.FuncHandler{
			Open:  func(uio.Conn) { c.open() },
			Data:  func(uio.Conn) error { return c.data() },
			Close: func(_ uio.Conn, err error) { c.closedBelow(err) },
		}
	})
	return perConn.Handler(next)
}
