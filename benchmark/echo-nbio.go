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

	// nbio 默认数量为 runtime.NumCPU() / 4
	// 如果未指定时强制统一为 runtime.NumCPU() 确保与其他框架一致
	if loops <= 0 {
		loops = runtime.NumCPU()
	}

	engine := nbio.NewEngine(nbio.Config{
		Network: "tcp",
		Addrs:   []string{fmt.Sprintf(":%d", port)},
		NPoller: loops,
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
