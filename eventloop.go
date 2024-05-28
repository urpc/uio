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
	"sync"
	"sync/atomic"

	"github.com/urpc/uio/internal/poller"
)

type eventLoop struct {
	events *Events
	poller *poller.NetPoller
	buffer []byte
	fdMap  map[int]*fdConn // all connections
	mux    sync.Mutex      // fdMap locker
}

func newEventLoop(events *Events) (*eventLoop, error) {
	p, err := poller.NewNetPoller()
	if nil != err {
		return nil, err
	}

	return &eventLoop{
			events: events,
			poller: p,
			buffer: make([]byte, events.MaxBufferSize),
			fdMap:  make(map[int]*fdConn, 1024),
		},
		nil
}

func (el *eventLoop) Serve(lockOSThread bool, handler poller.EventHandler) error {
	if nil == handler {
		handler = el
	}
	return el.poller.Serve(lockOSThread, handler)
}

func (el *eventLoop) Close(err error) error {
	el.mux.Lock()
	defer el.mux.Unlock()

	// close all connections
	for fd, fdc := range el.fdMap {
		delete(el.fdMap, fd)
		el.events.closeConn(fdc, err)
	}
	return el.poller.Close()
}

func (el *eventLoop) OnWrite(ep *poller.NetPoller, fd int) error {
	fdc := el.getConn(fd)
	if fdc == nil || 0 != atomic.LoadInt32(&fdc.closed) {
		return nil
	}
	return fdc.fireWriteEvent()
}

func (el *eventLoop) OnRead(ep *poller.NetPoller, fd int) error {
	fdc := el.getConn(fd)
	if fdc == nil || 0 != atomic.LoadInt32(&fdc.closed) {
		return nil
	}
	return fdc.fireReadEvent()
}

func (el *eventLoop) getBuffer() []byte {
	return el.buffer
}

func (el *eventLoop) getConn(fd int) *fdConn {
	el.mux.Lock()
	defer el.mux.Unlock()
	return el.fdMap[fd]
}

func (el *eventLoop) listen(fd int) error {
	return el.poller.AddRead(fd)
}

func (el *eventLoop) delConn(fdc *fdConn) {
	el.mux.Lock()
	defer el.mux.Unlock()
	delete(el.fdMap, fdc.Fd())
}

func (el *eventLoop) addConn(fdc *fdConn) error {
	el.mux.Lock()
	fd := fdc.Fd()
	el.fdMap[fd] = fdc
	el.mux.Unlock()
	return el.poller.AddRead(fd)
}

func (el *eventLoop) modRead(fdc *fdConn) error {
	return el.poller.ModRead(fdc.Fd())
}

func (el *eventLoop) modWrite(fdc *fdConn) error {
	return el.poller.ModWrite(fdc.Fd())
}

func (el *eventLoop) modReadWrite(fdc *fdConn) error {
	return el.poller.ModReadWrite(fdc.Fd())
}
