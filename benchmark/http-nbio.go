package main

import (
	"flag"
	"fmt"
	"net/http"
	"runtime"

	"github.com/lesismal/nbio/nbhttp"
	"github.com/lesismal/nbio/taskpool"
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

	mux := &http.ServeMux{}
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("hello world!"))
	})

	taskPool := taskpool.New(runtime.NumCPU()*1024, 1024*64)

	svr := nbhttp.NewEngine(nbhttp.Config{
		Network: "tcp",
		Addrs:   []string{fmt.Sprintf("localhost:%d", port)},
		Handler: mux,
		NPoller: loops,
	})
	svr.Execute = taskPool.Go

	err := svr.Start()
	if err != nil {
		fmt.Printf("nbio.Start failed: %v\n", err)
		return
	}
	defer svr.Stop()

	<-make(chan int)
}
