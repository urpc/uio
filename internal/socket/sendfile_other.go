//go:build !linux && !darwin && !freebsd && !dragonfly

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

import "syscall"

// SupportsSendfile reports whether the platform provides a kernel sendfile(2)
// implementation in this package.
const SupportsSendfile = false

// Sendfile is unsupported on this platform and returns ENOSYS so callers fall
// back to pooled user-space buffers.
func Sendfile(_, _ int, _ *int64, _ int) (int, error) {
	return 0, syscall.ENOSYS
}

// IsSendfileUnsupported reports whether err indicates that sendfile is
// unsupported.
func IsSendfileUnsupported(err error) bool {
	return err != nil
}

// IsSendfileSourceError reports an error sendfile(2) met reading its file;
// there is none without it.
func IsSendfileSourceError(error) bool { return false }
