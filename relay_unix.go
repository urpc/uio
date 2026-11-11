//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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

import "github.com/urpc/uio/internal/relay"

type relayGetter struct{}

// relayAccess gives internal/relay a native connection's splice relay; see
// fdConn.splicer.
type relayAccess = relay.Access[relayGetter]

func (relayGetter) GetSplicer(source any) relay.Splicer {
	if conn, ok := source.(*fdConn); ok {
		return conn.splicer()
	}
	return nil
}
