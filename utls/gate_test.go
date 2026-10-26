package utls

import (
	"github.com/urpc/uio"
)

// gate is a layer that runs a one-round handshake before it opens: both
// sides send HELLO?, answer with HELLO!, and open on the answer. It is a
// layer with a handshake under the TLS one.
var gate uio.Layer = uio.LayerFunc(func(uio.LayerContext) uio.LayerConn { return &gateLayer{} })

type gateLayer struct {
	uio.PassThrough
	pending []byte
	ready   bool
}

func (g *gateLayer) OnOpen(ctx uio.LayerContext) {
	_, _ = ctx.Write([]byte("HELLO?"))
}

func (g *gateLayer) OnInbound(ctx uio.LayerContext) error {
	if g.ready {
		return g.PassThrough.OnInbound(ctx)
	}
	lower := ctx.Lower()
	chunk := lower.PeekChunk()
	g.pending = append(g.pending, chunk...)
	_, _ = lower.Discard(len(chunk))
	for !g.ready && len(g.pending) >= len("HELLO?") {
		switch string(g.pending[:len("HELLO?")]) {
		case "HELLO?":
			g.pending = g.pending[len("HELLO?"):]
			_, _ = ctx.Write([]byte("HELLO!"))
		case "HELLO!":
			g.pending = g.pending[len("HELLO!"):]
			g.ready = true
		default:
			g.ready = true // not a gate peer: pass through unsettled
		}
	}
	if g.ready {
		ctx.Deliver(g.pending)
		g.pending = nil
		ctx.Open()
	}
	return nil
}
