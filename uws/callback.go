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

package uws

// UIO connection tasks serialize the complete protocol and application
// callback path for one connection. UWS therefore invokes handlers directly:
// no second mailbox, payload copy, or runner state is required here.
func (c *Conn) notifyOpen() {
	state := c.handshake.Load()
	defer c.releaseHandshakeState(state)
	if handler := c.handler; handler != nil {
		handler.OnOpen(c)
	}
}

func (c *Conn) notifyMessage(message Message) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if c.closing.Load() {
		// A close in progress drops the message without a callback; the read
		// round keeps consuming so the peer's Close completes the handshake.
		return nil
	}
	if handler := c.handler; handler != nil {
		handler.OnMessage(c, message)
	}
	return nil
}

func (c *Conn) notifyClose(info CloseEvent) {
	if handler := c.handler; handler != nil {
		handler.OnClose(c, info)
	}
}
