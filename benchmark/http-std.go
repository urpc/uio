package main

import (
	"flag"
	"fmt"
	"net/http"
	"runtime"
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

	//debug.SetMaxThreads(loops)

	mux := &http.ServeMux{}
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("hello world!"))
	})

	if err := http.ListenAndServe(fmt.Sprintf("localhost:%d", port), mux); nil != err {
		fmt.Println("server exited with error:", err)
	}
}
