//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

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

package socket

import (
	"errors"
	"syscall"
)

// AcceptFailure is what a listener does about an accept that failed.
type AcceptFailure int

const (
	// AcceptRetry: the failure belonged to one pending connection, which is
	// gone; the next accept may succeed at once.
	AcceptRetry AcceptFailure = iota
	// AcceptBackoff: the process or the system ran out of something the next
	// connection needs as well, such as descriptors or buffer memory, so
	// accepting resumes after a pause.
	AcceptBackoff
	// AcceptFatal: the listening descriptor itself is unusable.
	AcceptFatal
)

// ClassifyAcceptError sorts an error Accept returned. Errors it does not know
// back off: a listener stays up until it is closed.
func ClassifyAcceptError(err error) AcceptFailure {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return AcceptBackoff
	}
	switch errno {
	case syscall.EINTR, syscall.ECONNABORTED, syscall.ECONNRESET, syscall.EPROTO, syscall.EPERM:
		// EPERM: a firewall rule refused this connection.
		return AcceptRetry
	case syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM:
		return AcceptBackoff
	case syscall.EBADF, syscall.EINVAL, syscall.ENOTSOCK, syscall.EFAULT:
		return AcceptFatal
	}
	if pendingNetworkError(errno) {
		return AcceptRetry
	}
	return AcceptBackoff
}
