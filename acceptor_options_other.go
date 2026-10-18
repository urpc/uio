//go:build (darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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

import "net"

// inheritAcceptedOptions reports that this platform does not copy the
// listening socket's options to accepted sockets, so each accepted connection
// applies its own; the open turn does so through applyAcceptedOptions.
const inheritAcceptedOptions = false

// setListenerOptions has nothing to arm here.
func setListenerOptions(net.Listener) {}
