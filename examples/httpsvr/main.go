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

package main

import (
	"fmt"
	"net/http"

	"github.com/urpc/uio"
)

func main() {

	// 注册标准库http处理函数
	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		//fmt.Println("handle request:", request.URL.Path, request.RemoteAddr, time.Now().String())
		writer.Write([]byte("hello world!"))
	})

	// 使用标准库方案
	// wrk -t 1 -c 10 -d 10 --latency http://127.0.0.1:8081/
	go func() {
		http.ListenAndServe(":8081", nil)
	}()

	// 使用事件循环方案
	// wrk -t 1 -c 10 -d 10 --latency http://127.0.0.1:8080/
	ListenAndServe(":8080", nil)
}

func ListenAndServe(addr string, handler http.Handler) error {

	var events uio.Events
	events.Addrs = []string{addr}
	events.OnOpen = func(c uio.Conn) {
		//fmt.Println("connection opened:", c.RemoteAddr())
		c.SetContext(newHttpConn())
	}

	events.OnData = func(c uio.Conn) error {
		hConn := c.Context().(*httpConn)
		return hConn.ServeHTTP(c, handler)
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
