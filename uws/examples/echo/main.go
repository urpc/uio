package main

import (
	"log"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws"
)

type echoHandler struct{}

func (echoHandler) OnOpen(*uws.Conn) {}

func (echoHandler) OnMessage(conn *uws.Conn, message uws.Message) {
	switch message.Type {
	case uws.TextMessage:
		_ = conn.SendText(message.Payload)
	case uws.BinaryMessage:
		_ = conn.SendBinary(message.Payload)
	}
}

func (echoHandler) OnClose(*uws.Conn, uws.CloseEvent) {}

func main() {
	server := uws.NewServer(echoHandler{})
	server.Events = &uio.Events{Pollers: 4, MaxBufferSize: 4 << 10}
	if err := server.Serve(":19701"); err != nil {
		log.Fatal(err)
	}
}
