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

// tlsLayerOptions carries Options' handshake bound onto the TLS layers. A
// non-positive bound is the TLS layer's own default, as it was before the
// layers took their options as a struct.
func tlsLayerOptions(config *tls.Config, opts Options) utls.LayerOptions {
	timeout := opts.HandshakeTimeout
	if timeout < 0 {
		timeout = 0
	}
	return utls.LayerOptions{TLSConfig: config, HandshakeTimeout: timeout}
}

// serverTLSLayer is what TLSConfig installs on a Server: its listener's
// accepted connections speak TLS.
func serverTLSLayer(config *tls.Config, opts Options) uio.Layer {
	return utls.ServerLayer(tlsLayerOptions(config, opts))
}

// clientTLSLayer is the layer one wss:// dial brings; host is the name the
// dial verifies, unless the Config names its server.
func clientTLSLayer(config *tls.Config, opts Options, host string) uio.Layer {
	if config.ServerName == "" {
		cloned := config.Clone()
		cloned.ServerName = host
		config = cloned
	}
	return utls.ClientLayer(tlsLayerOptions(config, opts))
}
