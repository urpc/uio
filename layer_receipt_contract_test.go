package uio

import (
	"errors"
	"reflect"
	"testing"
)

// A stack with only receipts has no OnWireSent or Flush callbacks to run.
// After partial wire progress it must wait for another actual send report,
// rather than repeatedly requesting turns which cannot retire its receipt.
func TestLayerReceiptWaitsForWireProgressWithoutRescheduling(t *testing.T) {
	transport := &contractTransport{}
	var notices int
	c := contractStack(transport, FuncHandler{Sent: func(Conn, SendReceipt) { notices++ }}, passThroughHandler{})
	transport.queue = 9
	if _, _, err := c.topConn().(TrackedWriter).WriteTracked([]byte("ten-bytes!")); err != nil {
		t.Fatal(err)
	}
	stackSent(nil)(c.topConn(), 1)
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if notices != 0 || c.receiptCount.Load() != 1 {
		t.Fatal("partial wire progress retired an incomplete receipt")
	}
	if c.turnAsked.Load() || c.sentPending.Load() != 0 {
		t.Fatal("an incomplete receipt scheduled more turns without wire progress")
	}
	transport.queue = 0
	stackSent(nil)(c.topConn(), 9)
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if notices != 1 || c.receiptCount.Load() != 0 {
		t.Fatal("final queue drain did not retire its receipt")
	}
}

func TestLayerReceiptPreservesOrderAcrossReentrantWrites(t *testing.T) {
	transport := &contractTransport{}
	var positions []uint64
	var writeError error
	c := contractStack(transport, FuncHandler{Sent: func(conn Conn, r SendReceipt) {
		positions = append(positions, r.wire)
		if len(positions) == 1 {
			_, _, writeError = conn.(TrackedWriter).WriteTracked([]byte("ccc"))
		}
	}}, passThroughHandler{})
	transport.queue = 3
	for _, p := range []string{"a", "bb"} {
		if _, _, err := c.topConn().(TrackedWriter).WriteTracked([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	stackSent(nil)(c.topConn(), 1)
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if writeError != nil || !reflect.DeepEqual(positions, []uint64{1}) {
		t.Fatalf("first receipt/reentrant write = %v, %v", positions, writeError)
	}
	if c.receiptCount.Load() != 2 {
		t.Fatal("reentrant write replaced an older pending receipt")
	}
	transport.queue = 0
	stackSent(nil)(c.topConn(), 5)
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(positions, []uint64{1, 3, 6}) {
		t.Fatalf("receipt order = %v", positions)
	}
}

func TestLayerReceiptCloseStopsLaterNotifications(t *testing.T) {
	transport := &contractTransport{}
	cause := errors.New("receipt owner closed")
	var notices int
	c := contractStack(transport, FuncHandler{Sent: func(conn Conn, _ SendReceipt) { notices++; _ = conn.CloseWith(cause) }}, passThroughHandler{})
	for _, p := range []string{"first", "second"} {
		if _, _, err := c.topConn().(TrackedWriter).WriteTracked([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if notices != 1 || c.receiptCount.Load() != 0 {
		t.Fatal("receipt notification continued after its owner began closing")
	}
	c.runClosePlan()
	if !errors.Is(transport.closeCause, cause) {
		t.Fatalf("receipt close cause = %v", transport.closeCause)
	}
}

type receiptInputLayer struct{ PassThrough }

func (receiptInputLayer) OnSent(ctx LayerContext, _ SendReceipt) {
	_, _ = ctx.Up().Write([]byte("receipt input"))
}

func TestLayerReceiptProducedInputRunsOnALaterTurn(t *testing.T) {
	transport := &contractTransport{}
	var input []string
	c := contractStack(transport, FuncHandler{Read: func(conn Conn) error {
		if conn.InboundBuffered() != 0 {
			input = append(input, string(conn.PeekChunk()))
			_, _ = conn.Discard(-1)
		}
		return nil
	}}, passThroughHandler{}, receiptInputLayer{})
	bottom := c.layers[0]
	if _, _, err := bottom.WriteTracked([]byte("wire")); err != nil {
		t.Fatal(err)
	}
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if len(input) != 0 || !c.turnAsked.Load() {
		t.Fatal("receipt input was delivered reentrantly or never scheduled")
	}
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input, []string{"receipt input"}) {
		t.Fatalf("receipt-generated input = %v", input)
	}
}
