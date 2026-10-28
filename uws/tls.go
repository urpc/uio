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

package uws

import (
	"crypto/tls"

	"github.com/urpc/uio"
	"github.com/urpc/uio/utls"
)

// tlsLayer is the layer TLSConfig installs. One transport carries
// connections that need TLS and connections that do not — a Dialer's ws://
// and wss:// dials, a Server's own accepts and the plain HTTP/1.1 upgrades
// ServeHTTP adopts — so each connection picks TLS or nothing as it attaches,
// by what it carries.
type tlsLayer struct {
	config  *tls.Config
	options []utls.Option
	server  uio.Layer // a Server's accepts
	client  uio.Layer // a Dialer's wss:// dials, when the Config names its server
}

func newTLSLayer(config *tls.Config, opts Options, server bool) *tlsLayer {
	layer := &tlsLayer{config: config}
	if opts.HandshakeTimeout > 0 {
		layer.options = append(layer.options, utls.HandshakeTimeout(opts.HandshakeTimeout))
	}
	if server {
		layer.server = utls.Server(config, layer.options...)
	} else if config.ServerName != "" {
		layer.client = utls.Client(config, layer.options...)
	}
	return layer
}

func (l *tlsLayer) Attach(ctx uio.LayerContext) uio.LayerHandler {
	switch held := ctx.Down().Userdata().(type) {
	case *dialSetup:
		if !held.secure {
			return uio.PassThrough{}
		}
		if l.client != nil {
			return l.client.Attach(ctx)
		}
		// The Config names no server: verify the one the URL dialed.
		config := l.config.Clone()
		config.ServerName = held.serverName
		return utls.Client(config, l.options...).Attach(ctx)
	case *Conn:
		return uio.PassThrough{} // adopted from net/http, in the clear
	}
	if l.server != nil {
		return l.server.Attach(ctx)
	}
	return uio.PassThrough{}
}
