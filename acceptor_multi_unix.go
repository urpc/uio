//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"runtime"
	"sync"

	"github.com/urpc/uio/internal/poller"
)

// multiAcceptor spreads stream accepting over several goroutines when the
// caller enables ReusePort. Each acceptor owns one listener and one poller,
// which is the point: a listener's connections are never queued behind other
// listeners' drains. Sharing one epoll across many listeners made arriving
// connections wait out every listener ahead of theirs in the batch — with a
// per-listener drain of up to 64 connections that reached milliseconds — and
// the kernel hashes connections across a ReusePort group, so splitting the
// address over several listeners also splits that work over cores. Handing
// accepted connections to their loops is unchanged: every acceptor submits
// through the same path the master loop uses.
type multiAcceptor struct {
	polls []*poller.NetPoller
	lns   []*listener
	wg    sync.WaitGroup
}

// multiAcceptorsOverride fixes the acceptor count in tests; zero means
// automatic.
var multiAcceptorsOverride int

// testHookAcceptorThreadStarted runs at the top of every dedicated acceptor
// goroutine with the thread association that goroutine took, so a test can
// pin that Events.LockOSThread reaches the accept loops; see
// TestMultiAcceptorHonorsLockOSThread.
var testHookAcceptorThreadStarted func(locked bool)

// multiAcceptorCount returns how many acceptors share a ReusePort address.
// One acceptor per listener already removes the batch-tail wait; the extra
// ones divide the address's accepting work — a few microseconds of serial
// accept and hand-off per connection — over more cores, so the count grows
// with the machine, capped where a connection's own accept cost stops being
// the scarce resource and each acceptor's share would shrink its batches.
func multiAcceptorCount(procs int) int {
	if multiAcceptorsOverride > 0 {
		return multiAcceptorsOverride
	}
	count := procs / 12
	if count < 2 {
		count = 2
	}
	if count > 4 {
		count = 4
	}
	return count
}

// addMultiListen adds the remaining ReusePort listeners for l's address and
// registers one acceptor group for it. l is the first of them and is already
// created; the others bind the concrete address l resolved, so a port of zero
// still yields one shared port. Every listener joins the acceptor's registry,
// so close() releases them all. The acceptors themselves start later, from
// startMultiAcceptors — Serve calls it after OnStart has returned, and until
// then arriving connections wait in the listener backlogs.
func (ld *acceptor) addMultiListen(l *listener) error {
	count := multiAcceptorCount(runtime.GOMAXPROCS(0))
	ma := &multiAcceptor{}
	resolved := l.ln.Addr().String()

	fail := func(err error) error {
		for _, np := range ma.polls {
			_ = np.Close(nil)
		}
		return err
	}
	for i := 0; i < count; i++ {
		cur := l
		if i > 0 {
			var err error
			if cur, err = ld.listen(resolved, true); err != nil {
				// listen can fail after creating the listener (SetNonblock);
				// it is not in the registry yet, so release it here.
				if cur != nil {
					ld.closeListener(cur)
				}
				return fail(err)
			}
			ld.listeners[cur.fd] = cur
		}
		np, err := poller.NewNetPoller()
		if err != nil {
			return fail(err)
		}
		if err := np.Add(cur.fd, poller.Readable); err != nil {
			_ = np.Close(nil)
			return fail(err)
		}
		ma.polls = append(ma.polls, np)
		ma.lns = append(ma.lns, cur)
	}
	ld.multis = append(ld.multis, ma)
	return nil
}

// startMultiAcceptors starts one accept goroutine per listener of every
// ReusePort group. Serve calls it once OnStart has returned, so no connection
// is accepted — and no OnOpen can run — while the application is still
// initializing; the master loop, which accepts the single-listener form,
// begins polling at the same point for the same reason. The listeners are
// bound from setup time on, so a client that dials early waits in the backlog
// until accepting begins, exactly as it did before ReusePort spread accept.
func (ld *acceptor) startMultiAcceptors() {
	ld.mux.Lock()
	multis := ld.multis
	ld.mux.Unlock()
	for _, ma := range multis {
		for i := range ma.polls {
			ma.wg.Add(1)
			go ld.serveMulti(ma, i)
		}
	}
}

// serveMulti accepts l's share of the incoming connections until its poller
// is closed. One listener per poller, so any event on this poller is that
// listener's. The goroutine takes the thread association Events.LockOSThread
// asks for, as the master loop and the data waiters do.
func (ld *acceptor) serveMulti(ma *multiAcceptor, i int) {
	defer ma.wg.Done()
	locked := ld.events.LockOSThread
	if locked {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	if testHookAcceptorThreadStarted != nil {
		testHookAcceptorThreadStarted(locked)
	}
	ln := ma.lns[i]
	np := ma.polls[i]
	var evbuf [8]poller.Event
	for {
		n, err := np.Wait(evbuf[:], -1)
		if np.Closed() {
			return
		}
		if err != nil {
			ld.events.initiateClose(err)
			return
		}
		for j := 0; j < n; j++ {
			if evbuf[j].FD != ln.fd {
				continue
			}
			if err := ld.acceptStream(ln); err != nil {
				ld.events.initiateClose(err)
				return
			}
		}
	}
}
