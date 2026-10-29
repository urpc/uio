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

package ukcp

import (
	"github.com/urpc/uio"
)

// kcpLayer is what ServerLayer and ClientLayer build: the options every
// session through it runs with, and which side of the conversation it takes.
type kcpLayer struct {
	opts   Options
	client bool
	err    error // the options' fault, which ends every connection through it
}

var (
	_ uio.Layer       = (*kcpLayer)(nil)
	_ uio.PacketLayer = (*kcpLayer)(nil)
)

func newLayer(opts Options, client bool) uio.Layer {
	resolved, err := opts.resolve()
	return &kcpLayer{opts: resolved, client: client, err: err}
}

// newResolvedLayer builds the layer from options that already passed resolve.
// Resolving them twice would read a negative's normalized zero — KeepAlive
// off, the first segment's wait unbounded, fast retransmit disabled — as the
// "take the default" sentinel and put the defaults back.
func newResolvedLayer(resolved Options, client bool) uio.Layer {
	return &kcpLayer{opts: resolved, client: client}
}

// CarriesPackets marks the layer: it reads a packet connection's datagrams,
// one per turn, and hands the layers above the stream it makes of them.
func (l *kcpLayer) CarriesPackets() {}

// Attach starts one session. The LayerContext is kept: it stays valid until
// the connection's OnClose.
func (l *kcpLayer) Attach(ctx uio.LayerContext) uio.LayerHandler {
	return &conn{layer: l, ctx: ctx, opts: l.opts}
}
