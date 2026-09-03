package main

import (
	"log"
	"net/http"

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
	go func() {
		if err := server.Serve(); err != nil {
			log.Fatal(err)
		}
	}()

	http.Handle("/ws", server)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
