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
	"log"

	"github.com/urpc/uio/uws"
)

type echoHandler struct{}

const closeInternalError = 1011

func (echoHandler) OnOpen(*uws.Conn) {}

func (echoHandler) OnMessage(conn *uws.Conn, message uws.Message) {
	var err error
	switch message.Type {
	case uws.TextMessage:
		err = conn.SendText(message.Payload)
	case uws.BinaryMessage:
		err = conn.SendBinary(message.Payload)
	}
	if err != nil {
		_ = conn.Close(closeInternalError, "echo failed")
	}
}

func (echoHandler) OnClose(*uws.Conn, uws.CloseEvent) {}

func main() {
	server := uws.NewServer(uws.Options{
		Handler:           echoHandler{},
		Pollers:           4,
		ReadBufferSize:    4 << 10,
		EnableCompression: true,
	})
	if err := server.Serve(":19701"); err != nil {
		log.Fatal(err)
	}
}
