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

package uhttp

import (
	"fmt"
	"net/http"

	"github.com/urpc/uio"
)

func ListenAndServe(addr string, handler http.Handler) error {

	var events uio.Events
	events.Addrs = []string{addr}
	events.OnOpen = func(c uio.Conn) {
		//fmt.Println("connection opened:", c.RemoteAddr())
		c.SetContext(NewHttpConn(c, nil))
	}

	events.OnData = func(c uio.Conn) error {
		hConn := c.Context().(*HttpConn)
		return hConn.ServeHTTP(handler)
	}

	events.OnClose = func(c uio.Conn, err error) {
		//fmt.Println("connection closed:", c.RemoteAddr(), err)
	}

	if err := events.Serve(); nil != err {
		fmt.Println("server exited with error:", err)
		return err
	}
	return nil
}
