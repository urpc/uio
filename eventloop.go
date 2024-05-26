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
	"io"
	"sync/atomic"
	"syscall"

	cmap "github.com/orcaman/concurrent-map/v2"
	"github.com/urpc/uio/internal/poller"
	"golang.org/x/sys/unix"
)

type eventLoop struct {
	events      *Events
	poller      *poller.NetPoller
	buffer      []byte
	connections cmap.ConcurrentMap[int, *fdConn] // map[int]*fdConn // all connections
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
			connections: cmap.NewWithCustomShardingFunction[int, *fdConn](func(key int) uint32 {
				return uint32(key)
			}),
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
	// close all connections
	el.connections.IterCb(func(fd int, fc *fdConn) {
		el.events.closeConn(fc, err)
	})
	el.connections.Clear()
	return el.poller.Close()
}

func (el *eventLoop) OnWrite(ep *poller.NetPoller, fd int) error {

	fdc := el.getConn(fd)

	if fdc == nil || 0 != atomic.LoadInt32(&fdc.closed) {
		return nil
	}

	fdc.mux.Lock()

	for !fdc.outbound.Empty() {
		// writeable buffer
		data := fdc.outbound.Peek(el.buffer)

		if 0 == len(data) {
			panic("uio: buffer is too small")
		}

		// write buffer to fd.
		sent, err := unix.Write(fd, data)
		if nil != err {
			fdc.mux.Unlock()

			if errors.Is(err, syscall.EAGAIN) {
				return nil
			}

			el.events.delConn(fdc, err)
			return nil
		}

		// commit read offset.
		fdc.outbound.Discard(sent)
	}

	defer fdc.mux.Unlock()

	// outbound buffer is empty.
	return ep.ModRead(fd)
}

func (el *eventLoop) OnRead(ep *poller.NetPoller, fd int) error {

	fdc := el.getConn(fd)

	if fdc == nil || 0 != atomic.LoadInt32(&fdc.closed) {
		return nil
	}

	// read data from fd
	n, err := unix.Read(fd, el.buffer)
	if 0 == n || err != nil {
		if nil != err && errors.Is(err, syscall.EAGAIN) {
			return nil
		}
		// remote closed
		if nil == err {
			err = io.EOF
		}
		el.events.delConn(fdc, err)
		return nil
	}

	fdc.inboundTail = el.buffer[:n]

	// fire data callback.
	if err = el.events.onData(fdc); nil != err {
		el.events.delConn(fdc, err)
		return nil
	}

	// append tail bytes to inbound buffer
	if len(fdc.inboundTail) > 0 {
		_, _ = fdc.inbound.Write(fdc.inboundTail)
		fdc.inboundTail = fdc.inboundTail[:0]
	}

	return nil
}

func (el *eventLoop) getConn(fd int) *fdConn {
	if fc, ok := el.connections.Get(fd); ok {
		return fc
	}
	return nil
}

func (el *eventLoop) listen(fd int) error {
	return el.poller.AddRead(fd)
}

func (el *eventLoop) delConn(fdc *fdConn) {
	el.connections.Remove(fdc.fd)
}

func (el *eventLoop) addConn(fdc *fdConn) error {
	if el.connections.SetIfAbsent(fdc.fd, fdc) {
		return el.poller.AddRead(fdc.fd)
	}
	panic("uio: duplicate connection to add")
}

func (el *eventLoop) modWrite(fdc *fdConn) error {
	return el.poller.ModWrite(fdc.fd)
}

func (el *eventLoop) addReadWrite(fdc *fdConn) error {
	return el.poller.AddReadWrite(fdc.fd)
}
