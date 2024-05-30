package main

import (
	"flag"
	"fmt"
	"net/http"
	"runtime"

	"github.com/lesismal/nbio/taskpool"
	"github.com/urpc/uio"
	"github.com/urpc/uio/examples/uhttp"
)

func main() {
	var port int
	var loops int
	flag.IntVar(&port, "port", 9527, "server port")
	flag.IntVar(&loops, "loops", 0, "server loops")
	flag.Parse()

	// 如果未指定时强制统一为 runtime.NumCPU() 确保与其他框架一致
	if loops <= 0 {
		loops = runtime.NumCPU()
	}

	var mux = &http.ServeMux{}
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("hello world!"))
	})

	var taskPool = taskpool.New(runtime.NumCPU()*1024, 1024*64)

	var events uio.Events
	events.Pollers = loops
	events.Addrs = []string{fmt.Sprintf("localhost:%d", port)}

	events.OnOpen = func(c uio.Conn) {
		c.SetContext(uhttp.NewHttpConn(c, taskPool.Go)) // executor传nil代表原地执行
	}

	events.OnData = func(c uio.Conn) error {
		hConn := c.Context().(*uhttp.HttpConn)
		return hConn.ServeHTTP(mux)
	}

	fmt.Printf("uhttp server with loop=%d is listening on %s\n", events.Pollers, events.Addrs[0])

	if err := events.Serve(); nil != err {
		fmt.Println("server exited with error:", err)
	}
}
