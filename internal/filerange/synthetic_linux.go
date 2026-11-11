//go:build linux

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

package filerange

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// synthetic reports a regular file of a file system whose sizes are not
// the lengths of its files. f is regular, so Fd leaves its mode alone: only a
// pollable file is switched back to blocking.
func synthetic(f *os.File) bool {
	var fs unix.Statfs_t
	err := unix.Fstatfs(int(f.Fd()), &fs)
	runtime.KeepAlive(f)
	if err != nil {
		return false
	}
	switch uint32(fs.Type) {
	case unix.PROC_SUPER_MAGIC, unix.SYSFS_MAGIC, unix.DEBUGFS_MAGIC, unix.TRACEFS_MAGIC,
		unix.SECURITYFS_MAGIC, unix.CGROUP_SUPER_MAGIC, unix.CGROUP2_SUPER_MAGIC,
		unix.PSTOREFS_MAGIC, unix.EFIVARFS_MAGIC, unix.BPF_FS_MAGIC:
		return true
	}
	return false
}
