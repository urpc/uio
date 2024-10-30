//go:build windows || stdio

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

package poller

import (
	"fmt"
	"runtime"
)

type NetPoller struct {
	ev     chan int
	closed chan struct{}
	err    error
}

func NewNetPoller() (*NetPoller, error) {
	return &NetPoller{
		ev:     make(chan int),
		closed: make(chan struct{}),
	}, nil
}

func (ev *NetPoller) Serve(lockOSThread bool, handler EventHandler) error {

	if lockOSThread {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}

	for {
		select {
		case <-ev.closed:
			handler.OnClose(ev, ev.err)
			return ev.err
		case fd := <-ev.ev:
			handler.OnRead(ev, fd)
			handler.OnWrite(ev, fd)
		}
	}
}

func (ev *NetPoller) Close(err error) error {
	select {
	case <-ev.closed:
	default:
		ev.err = err
		close(ev.closed)
	}
	return nil
}

func (ev *NetPoller) AddRead(fd int) error {
	select {
	case <-ev.closed:
		return fmt.Errorf("poller closed")
	case ev.ev <- fd:
		return nil
	}
}

func (ev *NetPoller) ModRead(fd int) error {
	return nil
}

func (ev *NetPoller) ModWrite(fd int) error {
	return nil
}

func (ev *NetPoller) ModReadWrite(fd int) error {
	return nil
}
