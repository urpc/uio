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

package ulayer

import (
	"net"
	"time"

	"github.com/urpc/uio"
)

// Forward carries the uio.Conn methods a layer keeps as they are: the
// addressing, the userdata, the deadlines and the fd-shaped options of the
// connection below, handed on unchanged. Embed it in the conn a layer hands
// upward and define what the layer transforms — the reads, the writes, the
// buffered counts it holds bytes of its own for, and the closes, which are
// never Forward's to pass on: a layer's goodbye is its own.
type Forward struct {
	// Lower is the connection the layer reads and writes through.
	Lower uio.Conn
}

func (f Forward) LocalAddr() net.Addr         { return f.Lower.LocalAddr() }
func (f Forward) RemoteAddr() net.Addr        { return f.Lower.RemoteAddr() }
func (f Forward) Userdata() any               { return f.Lower.Userdata() }
func (f Forward) SetUserdata(value any)       { f.Lower.SetUserdata(value) }
func (f Forward) SetLinger(seconds int) error { return f.Lower.SetLinger(seconds) }
func (f Forward) SetNoDelay(noDelay bool) error {
	return f.Lower.SetNoDelay(noDelay)
}
func (f Forward) SetKeepAlive(keepAlive bool) error {
	return f.Lower.SetKeepAlive(keepAlive)
}
func (f Forward) SetKeepAlivePeriod(seconds int) error {
	return f.Lower.SetKeepAlivePeriod(seconds)
}
func (f Forward) SetReadBuffer(size int) error  { return f.Lower.SetReadBuffer(size) }
func (f Forward) SetWriteBuffer(size int) error { return f.Lower.SetWriteBuffer(size) }
func (f Forward) SetDeadline(deadline time.Time) error {
	return f.Lower.SetDeadline(deadline)
}
func (f Forward) SetReadDeadline(deadline time.Time) error {
	return f.Lower.SetReadDeadline(deadline)
}
func (f Forward) SetWriteDeadline(deadline time.Time) error {
	return f.Lower.SetWriteDeadline(deadline)
}
func (f Forward) Control(fn func(fd uintptr)) error {
	return f.Lower.Control(fn)
}

// Wake reaches the connection below, by the contract in the package
// documentation; a layer that must first pump its own protocol overrides it.
func (f Forward) Wake() error { return f.Lower.Wake() }
