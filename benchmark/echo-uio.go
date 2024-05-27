package main

import (
	"flag"
	"fmt"

	"github.com/urpc/uio"
)

func main() {
	var port int
	var loops int
	flag.IntVar(&port, "port", 9527, "server port")
	flag.IntVar(&loops, "loops", 0, "server loops")
	flag.Parse()

	var events uio.Events
	events.Pollers = loops
	events.Addrs = []string{fmt.Sprintf("tcp://:%d", port)}
	events.OnData = func(c uio.Conn) error {
		_, _ = c.WriteTo(c)
		return nil
	}

	fmt.Printf("uio echo server with loop=%d is listening on %s\n", events.Pollers, events.Addrs[0])

	if err := events.Serve(); nil != err {
		panic(fmt.Errorf("uio server exit, error: %v", err))
	}
}
