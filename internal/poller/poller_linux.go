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
	"errors"
	"runtime"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

const (
	readEvents  = unix.EPOLLIN
	writeEvents = unix.EPOLLOUT
	errorEvents = unix.EPOLLERR | unix.EPOLLHUP | unix.EPOLLRDHUP | unix.EPOLLPRI
)

type NetPoller struct {
	epfd   int   // epoll fd
	closed int32 // close flag
	err    error // close reason
}

func NewNetPoller() (*NetPoller, error) {
	fd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return &NetPoller{epfd: fd}, nil
}

func (ev *NetPoller) AddReadWrite(fd int) error {
	return unix.EpollCtl(
		ev.epfd,
		unix.EPOLL_CTL_ADD,
		fd,
		&unix.EpollEvent{
			Fd:     int32(fd),
			Events: readEvents | writeEvents | errorEvents,
		},
	)
}

func (ev *NetPoller) AddRead(fd int) error {
	return unix.EpollCtl(
		ev.epfd,
		unix.EPOLL_CTL_ADD,
		fd,
		&unix.EpollEvent{
			Fd:     int32(fd),
			Events: readEvents | errorEvents,
		},
	)
}

func (ev *NetPoller) ModRead(fd int) error {
	return unix.EpollCtl(
		ev.epfd,
		unix.EPOLL_CTL_MOD,
		fd,
		&unix.EpollEvent{
			Fd:     int32(fd),
			Events: readEvents | errorEvents,
		},
	)
}

func (ev *NetPoller) ModWrite(fd int) error {
	return unix.EpollCtl(
		ev.epfd,
		unix.EPOLL_CTL_MOD,
		fd,
		&unix.EpollEvent{
			Fd:     int32(fd),
			Events: writeEvents | errorEvents,
		},
	)
}

func (ev *NetPoller) ModReadWrite(fd int) error {
	return unix.EpollCtl(
		ev.epfd,
		unix.EPOLL_CTL_MOD,
		fd,
		&unix.EpollEvent{
			Fd:     int32(fd),
			Events: readEvents | writeEvents | errorEvents,
		},
	)
}

func (ev *NetPoller) Serve(lockOSThread bool, handler EventHandler) error {

	if lockOSThread {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}

	var events = make([]unix.EpollEvent, 1024)

	for {
		n, err := unix.EpollWait(ev.epfd, events, 100)
		switch {
		case n == 0 || (n < 0 && errors.Is(err, unix.EINTR)):
			continue
		case nil != err:
			handler.OnClose(ev, ev.err)
			return ev.err
		}

		for _, event := range events[:n] {
			if 0 != (event.Events & writeEvents) {
				handler.OnWrite(ev, int(event.Fd))
			}

			if 0 != (event.Events & (readEvents | errorEvents)) {
				handler.OnRead(ev, int(event.Fd))
			}
		}

	}
}

func (ev *NetPoller) Close(err error) error {
	if atomic.CompareAndSwapInt32(&ev.closed, 0, 1) {
		ev.err = err
		return unix.Close(ev.epfd)
	}
	return nil
}
