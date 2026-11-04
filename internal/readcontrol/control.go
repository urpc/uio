package readcontrol

import (
	"sync"
	"sync/atomic"
)

// Control counts the reasons one input boundary is paused. While any owner
// holds a pause the boundary's consumer callback does not run; the boundary
// keeps its input and decides for itself what else stops (a plain socket
// stops reading it, a layer stack keeps reading up to its bounds).
//
// Besides the count it keeps one fact for the boundary: a release whose
// resume turn has not reached the consumer yet (see Held). The end of the
// stream waits for that turn as it waits for a pause, so the consumer sees
// what it retained, and the end, before the connection closes. Neither
// stops a producer: what else a pause stops, what is retained and when the
// end may complete are the boundary's. The zero value is usable; New
// attaches the hooks. It must not be copied after its first use.
type Control struct {
	owners atomic.Int32
	owed   atomic.Bool           // a released pause's resume turn has not run
	manual atomic.Pointer[Pause] // the application's switch, created on first use
	check  func() error
	resume func() error
}

// New constructs a controller for an input boundary. check validates a new
// pause against the boundary's lifecycle; resume asks for a turn that hands
// retained input to the consumer once the last pause is released. Either may
// be nil. Both must be concurrency safe and return promptly, and resume must
// schedule work rather than run the consumer inline. Neither runs under an
// owner's lock.
func New(check func() error, resume func() error) *Control {
	return &Control{check: check, resume: resume}
}

// Check validates the boundary's lifecycle without changing any state.
func (c *Control) Check() error {
	if c == nil {
		return ErrUnsupported
	}
	if c.check != nil {
		return c.check()
	}
	return nil
}

func (c *Control) application() *Pause {
	if c == nil {
		return nil
	}
	if p := c.manual.Load(); p != nil {
		return p
	}
	p := c.NewPause()
	if c.manual.CompareAndSwap(nil, p) {
		return p
	}
	return c.manual.Load()
}

// PauseReads and ResumeReads operate the application's own owner. It is one
// switch, idempotent rather than reference counted.
func (c *Control) PauseReads() error  { return c.application().Pause() }
func (c *Control) ResumeReads() error { return c.application().Resume() }

// ReadsPaused reports whether any owner holds a pause.
func (c *Control) ReadsPaused() bool { return c != nil && c.owners.Load() != 0 }

// Held reports whether the end of the stream must wait for the consumer: a
// pause holds it, or a resume turn is owed.
func (c *Control) Held() bool { return c != nil && (c.owners.Load() != 0 || c.owed.Load()) }

// ConsumerRuns records the consumer's callback starting unpaused: any resume
// turn owed is this one.
func (c *Control) ConsumerRuns() {
	if c != nil && c.owed.Load() {
		c.owed.Store(false)
	}
}

// NewPause creates an independent owner of this control, initially unpaused.
func (c *Control) NewPause() *Pause {
	if c == nil {
		return nil
	}
	return &Pause{control: c}
}

// Pause owns one reason to pause an input boundary. Pause is idempotent for
// its owner and Resume releases only its own pause, never another owner's.
// Do not copy it.
type Pause struct {
	control *Control
	mu      sync.Mutex
	paused  atomic.Bool
}

// Pause holds this owner's pause. A closed or closing boundary refuses it.
func (p *Pause) Pause() error {
	if p == nil {
		return ErrUnsupported
	}
	if err := p.control.Check(); err != nil {
		return err
	}
	p.mu.Lock()
	if !p.paused.Load() {
		p.control.owners.Add(1)
		p.paused.Store(true)
	}
	p.mu.Unlock()
	return nil
}

// Resume releases this owner's pause; the last release asks the boundary
// for a turn that hands its retained input to the consumer. An owner that
// holds no pause changes nothing and schedules nothing: a callback that left
// input unconsumed without pausing asks for its next turn with Wake instead.
func (p *Pause) Resume() error {
	if p == nil {
		return ErrUnsupported
	}
	p.mu.Lock()
	last := false
	if p.paused.Load() {
		p.paused.Store(false)
		// Owed before the count reaches zero: a consumer that sees the
		// release clears it, never one that ran before it.
		p.control.owed.Store(true)
		last = p.control.owners.Add(-1) == 0
	}
	p.mu.Unlock()
	if last && p.control.resume != nil {
		return p.control.resume()
	}
	return nil
}

// Paused reports this owner's own pause, independent of other owners.
func (p *Pause) Paused() bool { return p != nil && p.paused.Load() }
