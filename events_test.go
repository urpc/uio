package uio

import (
	"fmt"
	"testing"
	"time"
)

func TestEvents(t *testing.T) {

	var events Events
	events.Pollers = 8
	events.Addrs = []string{
		"tcp://:9527",
	}

	events.OnOpen = func(c Conn) {
		//fmt.Println("new connection opened:", c.RemoteAddr())
	}

	events.OnData = func(c Conn) error {
		//fmt.Println("data received:", c.RemoteAddr(), len(data), "bytes received")
		_, _ = c.WriteTo(c)
		return nil
	}

	events.OnClose = func(c Conn, err error) {
		//fmt.Println("connection closed:", c.RemoteAddr(), err)
	}

	time.AfterFunc(time.Second*600, func() {
		events.Close(fmt.Errorf("shutdown"))
	})

	fmt.Println("events exit: ", events.Serve())
}
