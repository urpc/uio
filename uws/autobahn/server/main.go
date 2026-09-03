package main

import (
	"log"

	"github.com/urpc/uio"
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
	server := uws.NewServer(echoHandler{})
	server.Events = &uio.Events{
		Pollers:       4,
		MaxBufferSize: 4 << 10,
	}
	server.EnableCompression = true
	if err := server.Serve(":19701"); err != nil {
		log.Fatal(err)
	}
}
