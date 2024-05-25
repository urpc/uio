package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/panjf2000/gnet/v2"
)

type echoServer struct {
	gnet.BuiltinEventEngine

	eng  gnet.Engine
	addr string
	loop int
}

func (es *echoServer) OnBoot(eng gnet.Engine) gnet.Action {
	es.eng = eng
	log.Printf("echo server with loop=%d is listening on %s\n", es.loop, es.addr)
	return gnet.None
}

func (es *echoServer) OnTraffic(c gnet.Conn) gnet.Action {
	c.WriteTo(c)
	return gnet.None
}

func main() {
	var port int
	var loop int
	flag.IntVar(&port, "port", 9000, "--port 9000")
	flag.IntVar(&loop, "loops", 0, "server loops")
	flag.Parse()

	echo := &echoServer{addr: fmt.Sprintf("tcp://:%d", port), loop: loop}
	log.Println("gnet server exits:", gnet.Run(echo, echo.addr, gnet.WithNumEventLoop(loop)))
}
