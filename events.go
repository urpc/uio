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
	// The default value is runtime.NumCPU().
	Pollers int

	// Addrs is the listening addr list for a server.
	Addrs []string

	// ReusePort indicates whether to set up the SO_REUSEPORT socket option.
	// The default value is false.
	ReusePort bool

	// LockOSThread is used to determine whether each I/O event-loop is associated to an OS thread.
	// The default value is false.
	LockOSThread bool

	// MaxBufferSize is the maximum number of bytes that can be read from the remote when the readable event comes.
	// The default value is 4KB.
	MaxBufferSize int

	// WriteBufferedThreshold enabled when value is greater than 0, writes will go into the outbound buffer instead of attempting to send them out immediately,
	// unless the outbound buffer reaches the threshold or the flush function is manually called.
	//
	// If you have multiple Write call requirements, opening it will improve write performance because it reduces the number of system calls by merging multiple write operations to improve performance.
	// The default value is 0.
	WriteBufferedThreshold int

	// FullDuplex read events are always registered, which will improve program performance in most cases,
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

	// OnInbound when any bytes read by a socket, it triggers the inbound event.
	OnInbound func(c Conn, readBytes int)

	// OnOutbound when any bytes write to a socket, it triggers the outbound event.
	OnOutbound func(c Conn, writeBytes int)

	// OnStart it triggers on the server initialized.
	OnStart func(ev *Events)

	// OnStop it triggers on the server closed.
	OnStop func(ev *Events)
}

func (ev *Events) Serve(addrs ...string) (err error) {
	// append listen address.
	ev.Addrs = append(ev.Addrs, addrs...)

	// initialize events
	if err = ev.initEvents(); nil != err {
		return err
	}

	// trigger OnStart event.
	if ev.OnStart != nil {
		ev.OnStart(ev)
	}

	defer func() {
		// trigger OnStop event.
		if ev.OnStop != nil {
			ev.OnStop(ev)
		}
	}()

	// serve listener
	ev.waitGroup.Add(1)
	defer ev.waitGroup.Done()
	return ev.master.Serve(ev.LockOSThread, ev.acceptor)
}

func (ev *Events) Close(err error) error {
	// close events.
	ev.closeEvents(err)

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

func (ev *Events) closeEvents(err error) {
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
}

func (ev *Events) initConfig() error {

	if ev.Pollers <= 0 || ev.Pollers > runtime.NumCPU() {
		ev.Pollers = runtime.NumCPU()
	}

	if ev.MaxBufferSize <= 0 {
		ev.MaxBufferSize = 1024 * 4
	}

	if ev.WriteBufferedThreshold > 0 && ev.WriteBufferedThreshold < 1024 {
		ev.WriteBufferedThreshold = 1024
	}

	return nil
}

//go:norace
func (ev *Events) initLoops() (err error) {

	// create main loop
	if ev.master, err = newEventLoop(ev); nil != err {
		return err
	}

	ev.workers = make([]*eventLoop, ev.Pollers)
	for idx := range ev.workers {
		if ev.workers[idx], err = newEventLoop(ev); nil != err {
			return err
		}
	}

	for _, worker := range ev.workers {
		ev.waitGroup.Add(1)

		go func(worker *eventLoop) {
			defer ev.waitGroup.Done()
			worker.Serve(ev.LockOSThread, nil)
		}(worker)
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

//go:norace
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
		ev.closeConn(fdc, err)
		return err
	}

	return nil
}

func (ev *Events) closeConn(fdc *fdConn, err error) {

	// close socket and release resource.
	if fdc.fdClose(err) {
		// fire on-close event.
		if nil != ev.OnClose {
			ev.OnClose(fdc, err)
		}
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

func (ev *Events) onSocketBytesRead(fdc *fdConn, readBytes int) {
	if readBytes > 0 && ev.OnInbound != nil {
		ev.OnInbound(fdc, readBytes)
	}
}

func (ev *Events) onSocketBytesWrite(fdc *fdConn, writeBytes int) {
	if writeBytes > 0 && ev.OnOutbound != nil {
		ev.OnOutbound(fdc, writeBytes)
	}
}
