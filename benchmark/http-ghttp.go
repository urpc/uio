package main

import (
	"context"
	"flag"
	"fmt"
	"runtime"

	"github.com/lesismal/nbio/taskpool"
	"github.com/leslie-fei/ghttp/pkg/network"
	"github.com/leslie-fei/ghttp/pkg/network/gnet"
	"github.com/leslie-fei/ghttp/pkg/protocol"
	gnet2 "github.com/panjf2000/gnet/v2"
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

	taskPool := taskpool.New(runtime.NumCPU()*1024, 1024*64)

	addr := fmt.Sprintf("tcp://:%d", port)
	ts := gnet.NewTransporter(addr, gnet2.WithNumEventLoop(loops))

	srv := &protocol.Server{
		Handler: func(ctx *protocol.RequestCtx) {
			_, _ = ctx.Write([]byte("hello world!"))
		},
		Executor: taskPool.Go,
	}

	err := ts.ListenAndServe(func(ctx context.Context, conn interface{}) error {
		c := conn.(network.Conn)
		return srv.Serve(ctx, c)
	})

	if err != nil {
		fmt.Println("server exited with error:", err)
	}
}
