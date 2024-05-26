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
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
)

type Events struct {
	master      *eventLoop      // serving listener
	workers     []*eventLoop    // serving connection
	connections map[int]*fdConn // all connections
	acceptor    *acceptor       // connection acceptor
	waitGroup   sync.WaitGroup  // wait for all eventLoop exit on shutdown
	running     int32           // running state
	mux         sync.Mutex

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

	// OnOpen fires when a new connection has been opened.
	OnOpen func(c Conn)

	// OnData fires when a socket receives data from the remote.
	OnData func(c Conn) error

	// OnClose fires when a connection has been closed.
	OnClose func(c Conn, err error)
}

func (ev *Events) Serve() (err error) {

	if !atomic.CompareAndSwapInt32(&ev.running, 0, 1) {
		return errors.New("already running")
	}

	defer func() {
		// close loops.
		_ = ev.Close(err)
	}()

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

	// serve listener
	ev.waitGroup.Add(1)
	defer ev.waitGroup.Done()
	return ev.master.Serve(ev.LockOSThread, ev.acceptor)
}

func (ev *Events) Close(err error) error {

	if !atomic.CompareAndSwapInt32(&ev.running, 1, 0) {
		return nil // not running
	}

	ev.mux.Lock()
	defer ev.mux.Unlock()

	// stop main poller
	if nil != ev.master {
		_ = ev.master.Close()
	}

	// close all worker pollers
	for idx, worker := range ev.workers {
		if nil != worker {
			_ = worker.Close()
			ev.workers[idx] = nil
		}
	}

	// waiting for all event-loop exited.
	ev.waitGroup.Wait()

	// close all listeners
	ev.acceptor.close()

	// close all connections
	for fd, fc := range ev.connections {
		ev.closeConn(fc, err)
		delete(ev.connections, fd)
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

	ev.connections = make(map[int]*fdConn, 4096)
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
		listeners: make(map[int]*listener, len(ev.Addrs)),
		events:    ev,
	}

	for _, addr := range ev.Addrs {
		if err = ev.acceptor.addListen(addr); nil != err {
			return err
		}
	}

	return nil
}

func (ev *Events) getConn(fd int) *fdConn {
	ev.mux.Lock()
	defer ev.mux.Unlock()
	return ev.connections[fd]
}

func (ev *Events) selectLoop(fd int) *eventLoop {
	return ev.workers[fd%len(ev.workers)]
}

func (ev *Events) addConn(fdc *fdConn) error {

	ev.mux.Lock()
	ev.connections[fdc.fd] = fdc
	ev.mux.Unlock()

	// register to poller
	if err := fdc.loop.addRead(fdc.fd); err != nil {
		ev.delConn(fdc, err)
		return err
	}

	// fire on-open event callback
	if nil != ev.OnOpen {
		ev.OnOpen(fdc)
	}

	return nil
}

func (ev *Events) closeConn(fdc *fdConn, err error) {
	// store close reason
	fdc.err = err

	if nil != ev.OnClose {
		ev.OnClose(fdc, err)
	}

	fdc.mux.Lock()
	defer fdc.mux.Unlock()
	// close socket and release resource.
	_ = syscall.Close(fdc.fd)
	fdc.outbound.Reset()
	fdc.inbound.Reset()
	fdc.inboundTail = nil
}

func (ev *Events) delConn(fdc *fdConn, err error) {
	if atomic.CompareAndSwapInt32(&fdc.closed, 0, 1) {

		ev.mux.Lock()
		delete(ev.connections, fdc.fd)
		ev.mux.Unlock()

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
