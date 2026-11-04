package uws

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urpc/uio/internal/readcontrol"
)

// Fake transports use the same owner-aware controller as actual connections.
// A zero probe costs nothing until a test exercises explicit read control.
type probeReadControl struct {
	control atomic.Pointer[readcontrol.Control]
}

type probeReadAccess = readcontrol.Access[probeReadControlGetter]

type probeReadControlGetter struct{}

func (probeReadControlGetter) GetReadControl(source any) *readcontrol.Control {
	return source.(interface{ probeControl() *readcontrol.Control }).probeControl()
}

func (p *probeReadControl) get(resume func() error) *readcontrol.Control {
	if c := p.control.Load(); c != nil {
		return c
	}
	c := readcontrol.New(nil, resume)
	if p.control.CompareAndSwap(nil, c) {
		return c
	}
	return p.control.Load()
}

func (p *probeReadControl) ReadsPaused() bool {
	c := p.control.Load()
	return c != nil && c.ReadsPaused()
}

func (c *writeProbeConn) PauseReads() error  { return c.probeReadControl.get(c.Wake).PauseReads() }
func (c *writeProbeConn) ResumeReads() error { return c.probeReadControl.get(c.Wake).ResumeReads() }
func (c *writeProbeConn) ReadsPaused() bool  { return c.probeReadControl.ReadsPaused() }
func (c *writeProbeConn) probeControl() *readcontrol.Control {
	return c.probeReadControl.get(c.Wake)
}

func (c *echoPathConn) PauseReads() error  { return c.probeReadControl.get(nil).PauseReads() }
func (c *echoPathConn) ResumeReads() error { return c.probeReadControl.get(nil).ResumeReads() }
func (c *echoPathConn) ReadsPaused() bool  { return c.probeReadControl.ReadsPaused() }
func (c *echoPathConn) probeControl() *readcontrol.Control {
	return c.probeReadControl.get(nil)
}

func TestReadPauseUsesSharedBoundaryAndPreservesForeignOwners(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	foreign := readcontrol.NewPause(raw)
	if err := foreign.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := c.PauseReads(); err != nil {
		t.Fatal(err)
	}
	if !raw.ReadsPaused() {
		t.Fatal("WebSocket pause did not reach the shared input boundary")
	}
	if err := c.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	// The transport runs no callback while another owner holds it; the
	// WebSocket's own check sees its pauses only.
	if !foreign.Paused() || !raw.ReadsPaused() || c.readPaused.Load() != 0 {
		t.Fatal("WebSocket resume released another owner's pause")
	}
	if err := foreign.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if len(raw.inbound) != 0 || raw.ReadsPaused() {
		t.Fatal("last owner release did not restore frame consumption")
	}
}

// A resume without the pause it releases schedules nothing, so a pacing
// controller calling it from every OnOutbound costs no turns; a real release
// schedules one. Neither touches another owner's pause.
func TestIdleResumeReadsSchedulesNothing(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	for range 3 {
		if err := c.ResumeReads(); err != nil {
			t.Fatal(err)
		}
	}
	if raw.wakes != 0 || c.externalReadsPaused() || raw.ReadsPaused() {
		t.Fatalf("idle resume: wakes=%d, owner paused=%v, input paused=%v", raw.wakes, c.externalReadsPaused(), raw.ReadsPaused())
	}
	if err := c.PauseReads(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := c.ResumeReads(); err != nil {
			t.Fatal(err)
		}
	}
	if raw.wakes != 1 {
		t.Fatalf("one release scheduled %d turns, want one", raw.wakes)
	}
	foreign := readcontrol.NewPause(raw)
	if err := foreign.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := c.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if !foreign.Paused() || !raw.ReadsPaused() || raw.wakes != 1 {
		t.Fatal("an idle WebSocket resume touched a foreign pause")
	}
	if err := foreign.Resume(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseReleasesOnlyWebSocketReadPauseOwners(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	foreign := readcontrol.NewPause(raw)
	if err := foreign.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := c.PauseReads(); err != nil {
		t.Fatal(err)
	}
	if !c.setInternalReadPause(true) {
		t.Fatal("automatic pause was not acquired")
	}
	c.markClosing()
	if c.externalReadsPaused() || c.internalReadsPaused() {
		t.Fatal("local Close kept a WebSocket-owned pause")
	}
	if !foreign.Paused() || !raw.ReadsPaused() {
		t.Fatal("local Close released another controller's pause")
	}
	if err := foreign.Resume(); err != nil {
		t.Fatal(err)
	}
	if raw.ReadsPaused() {
		t.Fatal("closed WebSocket left an unowned pause behind")
	}
}

func TestCloseRacingReadPausesLeavesNoWebSocketPause(t *testing.T) {
	for range 100 {
		raw, c, _ := newBackpressureConn(64, 16)
		start := make(chan struct{})
		var done sync.WaitGroup
		done.Add(3)
		go func() {
			defer done.Done()
			<-start
			_ = c.PauseReads()
		}()
		go func() {
			defer done.Done()
			<-start
			c.setInternalReadPause(true)
		}()
		go func() {
			defer done.Done()
			<-start
			c.markClosing()
		}()
		close(start)
		done.Wait()
		if raw.ReadsPaused() || c.externalReadsPaused() || c.internalReadsPaused() {
			t.Fatal("a pause raced past local Close and blocked its handshake")
		}
	}
}
