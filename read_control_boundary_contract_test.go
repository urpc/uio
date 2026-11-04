package uio

import (
	"errors"
	"io"
	"net"
	"testing"

	"github.com/urpc/uio/internal/readcontrol"
)

// A stack ends with its socket — a truncated unit its layers could not
// complete ends the stream as a truncation — and a half-open close waits
// for everything the layers hold and the Handler's delivery. Pauses bound
// the socket input of a stack whose Events left MaxInboundBuffered off.
func TestStackEndWaitsForEveryLayerConsumer(t *testing.T) {
	transport := new(contractTransport)
	c := contractStack(transport, FuncHandler{}, passThroughHandler{}, passThroughHandler{})
	raw := new(fdConn)
	raw.entry = &entry{ev: new(Events), layered: true}
	raw.SetUserdata(c)
	top := c.layers[len(c.layers)-1]
	if c.ended() {
		t.Fatal("a stack ended before its socket did")
	}
	transport.state = StatePeerEnded
	if !c.ended() || !raw.readCloseReady() {
		t.Fatal("an empty stack behind an ended socket did not end")
	}
	c.layers[0].delivered = []byte("held by the intermediate consumer")
	// At the socket's end, data its consumer declined can never be completed:
	// the stack ends, and the end is a truncation rather than a pass-over.
	if !c.ended() || raw.readCloseReady() {
		t.Fatal("an intermediate layer's retained data was passed over")
	}
	if err := c.endOfStream(true); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a truncated stack ended with %v", err)
	}
	c.layers[0].delivered = nil
	c.layers[0].stalled.Store(true)
	if c.ended() || raw.readCloseReady() {
		t.Fatal("a layer that stopped short was passed over")
	}
	c.layers[0].stalled.Store(false)
	top.delivered = []byte("unread by the Handler")
	if !c.ended() {
		t.Fatal("the Handler's own unread delivery held the end back")
	}
	if raw.readCloseReady() {
		t.Fatal("a half-open close discarded the Handler's unread delivery")
	}
	top.delivered = nil
	transport.state = 0
	top.ended = true
	if !c.ended() {
		t.Fatal("the top layer's end did not end the stack")
	}
	if err := top.PauseReads(); err != nil {
		t.Fatal(err)
	}
	if err := c.endOfStream(true); err != nil {
		t.Fatal("a paused Handler did not hold the end back")
	}
	if got := raw.inputLimit(false); got != DefaultDeliverBound {
		t.Fatalf("paused stack input bound=%d", got)
	}
	if err := top.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if err := c.endOfStream(true); err != nil {
		t.Fatal("the end overtook the resume turn the release owes")
	}
	top.readControl.Load().ConsumerRuns()
	if err := c.endOfStream(true); err == nil {
		t.Fatal("an unpaused Handler kept the ended stack open")
	}
	if got := raw.inputLimit(false); got != 0 {
		t.Fatalf("unpaused default input bound=%d", got)
	}
	c.halfOpen = true
	if err := c.endOfStream(true); err != nil {
		t.Fatal("a half-open stack closed at its peer's end")
	}
	if c.yieldOwed() {
		t.Fatal("transport without yield capability reports a continuation")
	}
}

func TestReadControllerLookupAndClosedBoundaryContracts(t *testing.T) {
	getter := inputReadControlGetter{}
	if getter.GetReadControl(new(contractTransport)) != nil {
		t.Fatal("unsupported transport supplied a private controller")
	}
	var absent *layerNode
	if absent.inputReadControl() != nil {
		t.Fatal("nil layer supplied controller")
	}
	transport := new(contractTransport)
	c := contractStack(transport, FuncHandler{}, passThroughHandler{})
	top := c.topConn().(*layerNode)
	if got := top.inputReadControl(); got == nil || got != readcontrol.ControlOf(top) {
		t.Fatal("controller lookup lost boundary identity")
	}
	if (socketLower{Conn: top}).inputReadControl() != top.inputReadControl() {
		t.Fatal("socket wrapper supplied a separate pause owner count")
	}
	c.state.Store(stackClosed)
	if err := top.ResumeReads(); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}
