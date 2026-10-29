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

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// echoSendHandler echoes each message with the same type, through Send.
type echoSendHandler struct{}

func (echoSendHandler) OnOpen(*Conn) {}

func (echoSendHandler) OnMessage(c *Conn, message Message) {
	_ = c.Send(message.Type, message.Payload)
}

func (echoSendHandler) OnClose(*Conn, CloseEvent) {}

// Send frames a caller's bytes with the message type it is given: text goes
// out as a text message without a string conversion, binary as binary, and an
// unknown type is refused with ErrProtocol.
func TestSendFramesTheMessageType(t *testing.T) {
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: echoSendHandler{}})
	})
	defer stopTestServer(t, server, serveDone)

	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(1000, "") }()

	if err := client.Send(TextMessage, []byte("héllo")); err != nil {
		t.Fatalf("Send text: %v", err)
	}
	text, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read the text echo: %v", err)
	}
	if text.Type != TextMessage || string(text.Payload) != "héllo" {
		t.Fatalf("the text echo came back %d %q", text.Type, text.Payload)
	}

	binary := []byte{0x00, 0xff, 0x01, 0x80}
	if err := client.Send(BinaryMessage, binary); err != nil {
		t.Fatalf("Send binary: %v", err)
	}
	binaryEcho, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read the binary echo: %v", err)
	}
	if binaryEcho.Type != BinaryMessage || !bytes.Equal(binaryEcho.Payload, binary) {
		t.Fatalf("the binary echo came back %d %x", binaryEcho.Type, binaryEcho.Payload)
	}

	if err := client.Send(MessageType(99), []byte("x")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Send with an unknown type = %v, want %v", err, ErrProtocol)
	}
}
