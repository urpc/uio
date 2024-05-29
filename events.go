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

package uio

import (
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/urpc/uio/internal/bytebuf"
)

type CompositeBuffer = bytebuf.CompositeBuffer

type Events struct {
	master    *eventLoop     // serving listener
	workers   []*eventLoop   // serving connection
	acceptor  *acceptor      // connection acceptor
	waitGroup sync.WaitGroup // wait for all eventLoop exit on shutdown
	mux       sync.Mutex

	// Pollers is set up to start the given number of event-loop goroutine.
	Pollers int

	// Addrs is the listening addr list for a server.
	Addrs []string

	// ReusePort indicates whether to set up the SO_REUSEPORT socket option.
	ReusePort bool

	// LockOSThread is used to determine whether each I/O event-loop is associated to an OS thread.
	LockOSThread bool

	// MaxBufferSize is the maximum number of bytes that can be read from the remote when the readable event comes.
	// The default value is 64KB.
	MaxBufferSize int

	// Read events are always registered, which will improve program performance in most cases,
	// unfortunately if there are malicious clients may deliberately slow to receive data will lead to server outbound buffer accumulation,
	// in the absence of intervention may lead to service memory depletion.
	// Turning this option off will change the event registration policy, readable events are unregistered if there is currently data waiting to be sent in the outbound buffer.
	// Readable events are re-registered after the send buffer has been sent. In this case, network reads and writes will degenerate into half-duplex mode, ensuring that server memory is not exhausted.
	// The default value is false.
	FullDuplex bool

	// OnOpen fires when a new connection has been opened.
	OnOpen func(c Conn)

	// OnData fires when a socket receives data from the remote.
	OnData func(c Conn) error

	// OnClose fires when a connection has been closed.
	OnClose func(c Conn, err error)
}

func (ev *Events) Serve() (err error) {

	defer func() {
		// close loops.
		_ = ev.Close(err)
	}()

	// initialize events
	if err = ev.initEvents(); nil != err {
		return err
	}

	// serve listener
	ev.waitGroup.Add(1)
	defer ev.waitGroup.Done()
	return ev.master.Serve(ev.LockOSThread, ev.acceptor)
}

func (ev *Events) Close(err error) error {
	ev.mux.Lock()
	defer ev.mux.Unlock()

	// close all listeners
	ev.acceptor.close()

	// stop main poller
	if nil != ev.master {
		_ = ev.master.Close(err)
	}

	// close all worker pollers
	for _, worker := range ev.workers {
		if nil != worker {
			_ = worker.Close(err)
			//ev.workers[idx] = nil
		}
	}

	// waiting for all event-loop exited.
	ev.waitGroup.Wait()

	return nil
}

func (ev *Events) initEvents() (err error) {
	ev.mux.Lock()
	defer ev.mux.Unlock()

	// init configs.
	if err = ev.initConfig(); nil != err {
		return err
	}

	// init event loops.
	if err = ev.initLoops(); nil != err {
		return err
	}

	// init listener.
	if err = ev.initListeners(); nil != err {
		return err
	}

	return nil
}

func (ev *Events) initConfig() error {

	if ev.Pollers <= 0 || ev.Pollers > runtime.NumCPU() {
		ev.Pollers = runtime.NumCPU()
	}

	if ev.MaxBufferSize <= 0 {
		ev.MaxBufferSize = 1024 * 64
	}

	return nil
}

func (ev *Events) initLoops() (err error) {

	// create main loop
	if ev.master, err = newEventLoop(ev); nil != err {
		return err
	}

	ev.workers = make([]*eventLoop, ev.Pollers)
	for i := 0; i < ev.Pollers; i++ {
		if ev.workers[i], err = newEventLoop(ev); nil != err {
			return err
		}

		ev.waitGroup.Add(1)

		go func(i int) {
			defer ev.waitGroup.Done()
			ev.workers[i].Serve(ev.LockOSThread, nil)
		}(i)
	}

	return nil
}

func (ev *Events) initListeners() (err error) {

	ev.acceptor = &acceptor{
		loop:   ev.master,
		events: ev,
	}

	for _, addr := range ev.Addrs {
		if err = ev.acceptor.addListen(addr); nil != err {
			return err
		}
	}

	return nil
}

func (ev *Events) selectLoop(fd int) *eventLoop {
	return ev.workers[fd%len(ev.workers)]
}

func (ev *Events) addConn(fdc *fdConn) error {

	// fire on-open event callback
	if nil != ev.OnOpen {
		ev.OnOpen(fdc)
	}

	// register to poller
	if err := fdc.loop.addConn(fdc); err != nil {
		ev.delConn(fdc, err)
		return err
	}

	return nil
}

func (ev *Events) closeConn(fdc *fdConn, err error) {

	// close socket and release resource.
	fdc.fdClose(err)

	// fire on-close event.
	if nil != ev.OnClose {
		ev.OnClose(fdc, err)
	}
}

func (ev *Events) delConn(fdc *fdConn, err error) {
	if atomic.CompareAndSwapInt32(&fdc.closed, 0, 1) {
		fdc.loop.delConn(fdc)
		ev.closeConn(fdc, err)
	}
}

func (ev *Events) onData(fdc *fdConn) error {
	if nil != ev.OnData {
		return ev.OnData(fdc)
	}
	// discard all received bytes if not set OnData.
	//
	_, _ = fdc.Discard(-1)
	return nil
}
