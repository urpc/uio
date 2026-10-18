//go:build (linux && race) || darwin || netbsd || freebsd || openbsd || dragonfly

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

// Recv reads from a connected stream socket. Race builds keep the syscall
// package's race annotations, and the BSDs keep read.
func Recv(fd int, p []byte) (int, error) { return syscall.Read(fd, p) }

// Send writes to a connected stream socket.
func Send(fd int, p []byte) (int, error) { return syscall.Write(fd, p) }
