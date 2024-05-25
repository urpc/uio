package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/tidwall/evio"
)

func main() {
	var port int
	var loops int

	flag.IntVar(&port, "port", 5000, "server port")
	flag.IntVar(&loops, "loops", 0, "num loops")
	flag.Parse()

	var events evio.Events
	events.NumLoops = loops
	events.Serving = func(srv evio.Server) (action evio.Action) {
		log.Printf("echo server started on port %d (event-loops: %d)", port, srv.NumLoops)
		return
	}

	events.Data = func(c evio.Conn, in []byte) (out []byte, action evio.Action) {
		return in, evio.None
	}
	log.Fatal(evio.Serve(events, fmt.Sprintf("tcp://127.0.0.1:%d", port)))
}
