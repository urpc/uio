//go:build !linux

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

// SplicePipe is a placeholder for Linux splice pipes on non-Linux platforms.
type SplicePipe struct {
	R int
	W int
}

// AcquireSplicePipe returns ENOSYS on non-Linux platforms.
func AcquireSplicePipe() (*SplicePipe, error) {
	return nil, syscall.ENOSYS
}

// ReleaseSplicePipe is a no-op on non-Linux platforms.
func ReleaseSplicePipe(*SplicePipe) {}

// CloseSplicePipe is a no-op on non-Linux platforms.
func CloseSplicePipe(*SplicePipe) {}

// Splice returns ENOSYS on non-Linux platforms (or (0, nil) when count <= 0).
func Splice(_ int, _ *int64, _ int, _ *int64, count int) (int, error) {
	if count <= 0 {
		return 0, nil
	}
	return 0, syscall.ENOSYS
}

// ReadSplicePipe returns ENOSYS on non-Linux platforms.
func ReadSplicePipe(_ int, _ []byte) (int, error) {
	return 0, syscall.ENOSYS
}

// IsSpliceUnsupported reports whether err indicates splice is unsupported.
func IsSpliceUnsupported(err error) bool {
	return err != nil
}
