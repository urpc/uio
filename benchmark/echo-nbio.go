package main

import (
	"flag"
	"fmt"
	"runtime"

	"github.com/lesismal/nbio"
)

func main() {
	var port int
	var loops int
	flag.IntVar(&port, "port", 9527, "server port")
	flag.IntVar(&loops, "loops", 0, "server loops")
	flag.Parse()

	if loops <= 0 {
		loops = runtime.NumCPU()
	}

	engine := nbio.NewEngine(nbio.Config{
		Network:                      "tcp",
		Addrs:                        []string{fmt.Sprintf(":%d", port)},
		MaxWriteBufferSize:           64 * 1024 * 1024,
		NPoller:                      loops,
		MaxConnReadTimesPerEventLoop: 1,
	})

	engine.OnData(func(c *nbio.Conn, data []byte) {
		c.Write(data)
	})

	err := engine.Start()
	if err != nil {
		fmt.Printf("nbio.Start failed: %v\n", err)
		return
	}
	defer engine.Stop()

	<-make(chan int)
}
