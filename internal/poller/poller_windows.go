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

import "fmt"

type NetPoller struct {
	ev     chan int
	closed chan struct{}
}

func NewNetPoller() (*NetPoller, error) {
	return &NetPoller{
		ev:     make(chan int),
		closed: make(chan struct{}),
	}, nil
}

func (ev *NetPoller) Serve(lockOSThread bool, handler EventHandler) error {

	for {
		select {
		case <-ev.closed:
			return nil
		case fd := <-ev.ev:
			_ = handler.OnRead(ev, fd)
			_ = handler.OnWrite(ev, fd)
		}
	}

}

func (ev *NetPoller) Close() error {
	select {
	case <-ev.closed:
	default:
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
