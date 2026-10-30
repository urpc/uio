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

// Command echosvr echoes every connection, prints its lifecycle, and shows
// the smallest working handler: WriteTo(c) moves what arrived straight back.
package main

import (
	"fmt"

	"github.com/urpc/uio"
)

func main() {
	var events uio.Events

	events.Handler = uio.FuncHandler{
		Open: func(c uio.Conn) {
			fmt.Println("connection opened:", c.RemoteAddr())
		},
		Read: func(c uio.Conn) error {
			// WriteTo(c) writes the inbound bytes back to this same
			// connection: the smallest echo, and what the load tools run.
			_, err := c.WriteTo(c)
			return err
		},
		Close: func(c uio.Conn, err error) {
			fmt.Println("connection closed:", c.RemoteAddr())
		},
	}

	fmt.Println("echo on :9527")
	if err := events.Serve(":9527"); nil != err {
		fmt.Println("server exited with error:", err)
	}
}
