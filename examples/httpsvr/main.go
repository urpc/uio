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

// Command httpsvr serves net/http handlers on uio: uhttp is a small HTTP/1.1
// server whose connections are uio's, so no goroutine serves a request.
package main

import (
	"fmt"
	"net/http"

	"github.com/urpc/uio/examples/httpsvr/uhttp"
)

func main() {
	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("hello world!"))
	})

	// uhttp runs net/http handlers on uio connections: the handler above is
	// reached with no goroutine and no net/http server of its own.
	fmt.Println("http on :8080")
	if err := uhttp.ListenAndServe(":8080", nil); err != nil {
		fmt.Println("server exited with error:", err)
	}
}
