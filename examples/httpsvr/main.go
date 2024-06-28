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
	"net/http"

	"github.com/urpc/uio/examples/httpsvr/uhttp"
)

func main() {

	// register http handler to http.DefaultServeMux
	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		//fmt.Println("handle request:", request.URL.Path, request.RemoteAddr, time.Now().String())
		writer.Write([]byte("hello world!"))
	})

	// serve uhttp server.
	uhttp.ListenAndServe(":8080", nil)
}
