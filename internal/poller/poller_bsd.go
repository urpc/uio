//go:build darwin || netbsd || freebsd || openbsd || dragonfly

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
	"runtime"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const (
	readEvents  = unix.EVFILT_READ
	writeEvents = unix.EVFILT_WRITE
	errorEvents = unix.EV_EOF | unix.EV_ERROR
)

type NetPoller struct {
	kqfd   int
	closed int32
}

func NewNetPoller() (*NetPoller, error) {
	fd, err := unix.Kqueue()
	if nil != err {
		return nil, err
	}
	return &NetPoller{kqfd: fd}, nil
}

func (ev *NetPoller) AddRead(fd int) error {
	_, err := unix.Kevent(
		ev.kqfd,
		[]unix.Kevent_t{
			{Ident: uint64(fd), Flags: unix.EV_ADD, Filter: readEvents},
		},
		nil,
		nil,
	)
	return err
}

func (ev *NetPoller) ModRead(fd int) error {
	_, err := unix.Kevent(
		ev.kqfd,
		[]unix.Kevent_t{
			{Ident: uint64(fd), Flags: unix.EV_DELETE, Filter: writeEvents},
		},
		nil,
		nil,
	)
	return err
}

func (ev *NetPoller) ModWrite(fd int) error {
	_, err := unix.Kevent(
		ev.kqfd,
		[]unix.Kevent_t{
			{Ident: uint64(fd), Flags: unix.EV_ADD, Filter: readEvents},
			{Ident: uint64(fd), Flags: unix.EV_ADD, Filter: writeEvents},
		},
		nil,
		nil,
	)
	return err
}

func (ev *NetPoller) Serve(lockOSThread bool, handler EventHandler) error {

	if lockOSThread {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}

	var timeout unix.Timespec
	timeout.Sec = 0
	timeout.Nsec = int64(time.Millisecond * 100)

	var events = make([]unix.Kevent_t, 1024)

	for {
		n, err := unix.Kevent(ev.kqfd, nil, events, &timeout)
		switch {
		case n == 0 || (n < 0 && errors.Is(err, unix.EINTR)):
			continue
		case nil != err:
			if 0 != atomic.LoadInt32(&ev.closed) {
				return nil
			}
			_ = ev.Close()
			return err
		}

		for i := 0; i < n; i++ {
			var event = &events[i]

			switch {
			case event.Filter == writeEvents:
				if err = handler.OnWrite(ev, int(event.Ident)); nil != err {
					_ = ev.Close()
					return err
				}
			case event.Filter == readEvents || (0 != event.Flags&errorEvents):
				if err = handler.OnRead(ev, int(event.Ident)); nil != err {
					_ = ev.Close()
					return err
				}
			}
		}
	}
}

func (ev *NetPoller) Close() error {
	if atomic.CompareAndSwapInt32(&ev.closed, 0, 1) {
		return unix.Close(ev.kqfd)
	}
	return nil
}
