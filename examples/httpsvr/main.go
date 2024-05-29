package main

import (
	"fmt"
	"net/http"

	"github.com/urpc/uio"
)

func main() {

	// 注册标准库http处理函数
	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		//fmt.Println("handle request:", request.URL.Path, request.RemoteAddr, time.Now().String())
		writer.Write([]byte("hello world!"))
	})

	// 使用标准库方案
	// wrk -t 1 -c 10 -d 10 --latency http://127.0.0.1:8081/
	go func() {
		http.ListenAndServe(":8081", nil)
	}()

	// 使用事件循环方案
	// wrk -t 1 -c 10 -d 10 --latency http://127.0.0.1:8080/
	var events uio.Events
	events.Addrs = []string{fmt.Sprintf("tcp://0.0.0.0:%d", 8080)}
	events.OnOpen = func(c uio.Conn) {
		fmt.Println("connection opened:", c.RemoteAddr())
		c.SetContext(newHttpConn())
	}

	events.OnData = func(c uio.Conn) error {
		hConn := c.Context().(*httpConn)
		return hConn.ServeHTTP(c, http.DefaultServeMux)
	}

	events.OnClose = func(c uio.Conn, err error) {
		fmt.Println("connection closed:", c.RemoteAddr(), err)
	}

	if err := events.Serve(); nil != err {
		fmt.Println("server exited with error:", err)
	}
}
