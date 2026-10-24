//go:build linux || darwin

package uio

import (
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// exhaustDescriptors lowers the process's descriptor limit to what is open
// now and fills any gaps below it, so the next descriptor anything allocates
// fails with EMFILE. The returned function frees them and restores the limit.
func exhaustDescriptors(t *testing.T) (restore func()) {
	t.Helper()
	var saved syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &saved); err != nil {
		t.Fatal(err)
	}
	highest := 0
	for fd := 0; fd < 1<<14; fd++ {
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) == nil {
			highest = fd
		}
	}
	lowered := saved
	lowered.Cur = uint64(highest + 1)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lowered); err != nil {
		t.Fatal(err)
	}
	var fillers []int
	for {
		fd, err := syscall.Open(os.DevNull, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err != nil {
			break
		}
		fillers = append(fillers, fd)
	}
	return func() {
		for _, fd := range fillers {
			_ = syscall.Close(fd)
		}
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &saved)
	}
}

// Running out of descriptors while connections wait to be accepted neither
// stops the Events nor spins the loop that accepts: the listener pauses, and
// once descriptors are free again it accepts the connections still waiting
// and new ones.
func TestAcceptSurvivesDescriptorExhaustion(t *testing.T) {
	const waiting = 4
	type startState struct {
		clients []net.Conn
		restore func()
	}
	opened := make(chan Conn, waiting+1)
	started := make(chan startState, 1)
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Open: func(c Conn) { opened <- c },
		Start: func(ev *Events) {
			// Nothing is accepted until OnStart returns: the clients wait in
			// the backlog, and the first accept finds no descriptor left.
			var state startState
			defer func() { started <- state }()
			addr := ""
			for _, l := range ev.acceptor.listeners {
				addr = l.ln.Addr().String()
			}
			for range waiting {
				c, err := net.Dial("tcp4", addr)
				if err != nil {
					t.Error(err)
					return
				}
				state.clients = append(state.clients, c)
			}
			state.restore = exhaustDescriptors(t)
		},
	})
	var serveErr error
	served := make(chan struct{})
	go func() {
		serveErr = events.Serve("tcp4://127.0.0.1:0")
		close(served)
	}()
	defer func() {
		_ = events.Close(nil)
		<-served
	}()
	var state startState
	select {
	case state = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("OnStart did not run")
	}
	clients := state.clients
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	if state.restore == nil {
		t.FailNow()
	}
	restored := false
	defer func() {
		if !restored {
			state.restore()
		}
	}()

	time.Sleep(50 * time.Millisecond) // the shortage has begun
	cpuBefore, wallBefore := processCPUTime(), time.Now()
	time.Sleep(300 * time.Millisecond)
	cpu, wall := processCPUTime()-cpuBefore, time.Since(wallBefore)
	select {
	case <-served:
		t.Fatalf("Serve returned while descriptors ran out: %v", serveErr)
	default:
	}
	if len(opened) != 0 {
		t.Fatalf("%d connections opened without a descriptor to spare", len(opened))
	}
	state.restore()
	restored = true
	if cpu > wall/2 {
		t.Errorf("the process used %v of CPU in %v while accepting could not proceed", cpu, wall)
	}

	// Linux keeps a connection whose accept ran out of descriptors in the
	// backlog, where the paused listener finds it again; the BSDs drop it.
	if runtime.GOOS == "linux" {
		for i := range waiting {
			select {
			case <-opened:
			case <-time.After(5 * time.Second):
				t.Fatalf("%d of %d waiting connections opened once descriptors were free", i, waiting)
			}
		}
	}
	addr := ""
	events.acceptor.mux.Lock()
	for _, l := range events.acceptor.listeners {
		addr = l.ln.Addr().String()
	}
	events.acceptor.mux.Unlock()
	late, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	clients = append(clients, late)
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("a new connection did not open after the shortage")
	}
}
