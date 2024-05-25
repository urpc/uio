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
	events.Addrs = append(events.Addrs, fmt.Sprintf("tcp://:%d", port))
	events.OnData = func(c uio.Conn) error {
		_, _ = c.WriteTo(c)
		return nil
	}

	fmt.Printf("echo server with loop=%d is listening on %s\n", loops, events.Addrs[0])

	if err := events.Serve(); nil != err {
		panic(fmt.Errorf("ultraio server exit, error: %v", err))
	}

}
