/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package uhttp

import (
	"bufio"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/antlabs/httparser"
	"github.com/urpc/uio"
	"golang.org/x/net/http/httpguts"
)

var bufWriterPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewWriterSize(nil, 1024)
	},
}

type ReqExecutor func(fn func())

type HttpConn struct {
	conn       uio.Conn
	remoteAddr string
	executor   ReqExecutor
	parser     *httparser.Parser
	buffer     []byte
	request    *http.Request
	writer     *httpResponseWriter
	lastHeader string
	finished   bool
}

func NewHttpConn(conn uio.Conn, executor ReqExecutor) *HttpConn {

	hConn := &HttpConn{
		conn:       conn,
		remoteAddr: conn.RemoteAddr().String(),
		executor:   executor,
		parser:     httparser.New(httparser.REQUEST),
		buffer:     make([]byte, 1024),
		request:    &http.Request{Header: make(http.Header), Body: http.NoBody},
		writer:     &httpResponseWriter{},
	}
	hConn.parser.SetUserData(hConn)
	return hConn
}

func (hc *HttpConn) ServeHTTP(handler http.Handler) error {

	request, err := hc.parseHttpRequest()
	if nil != err {
		return err
	}

	if nil != request {
		if nil != hc.executor {
			// handle request in executor
			hc.executor(func() {
				_ = hc.Handle(handler)
			})
			return nil
		}
		return hc.Handle(handler)
	}

	return nil
}

func (hc *HttpConn) parseHttpRequest() (r *http.Request, err error) {

	// 获取当前已接受但是未处理的数据
	buffer := hc.conn.Peek(hc.buffer)

	// 解析http数据
	parsedBytes, err := hc.parser.Execute(httpParserSettings, buffer)
	if nil != err {
		return nil, err
	}

	// 成功解析部分或者全部数据
	if parsedBytes > 0 {
		_, _ = hc.conn.Discard(parsedBytes)
	}

	// 成功解析一个请求
	if hc.finished {
		request := hc.request
		rawurl := request.RequestURI

		// CONNECT requests are used two different ways, and neither uses a full URL:
		// The standard use is to tunnel HTTPS through an HTTP proxy.
		// It looks like "CONNECT www.google.com:443 HTTP/1.1", and the parameter is
		// just the authority section of a URL. This information should go in req.URL.Host.
		//
		// The net/rpc package also uses CONNECT, but there the parameter is a path
		// that starts with a slash. It can be parsed with the regular URL parser,
		// and the path will end up in req.URL.Path, where it needs to be in order for
		// RPC to work.
		justAuthority := request.Method == "CONNECT" && !strings.HasPrefix(rawurl, "/")
		if justAuthority {
			rawurl = "http://" + rawurl
		}

		if request.URL, err = url.ParseRequestURI(rawurl); nil != err {
			return nil, err
		}

		if justAuthority {
			// Strip the bogus "http://" back off.
			request.URL.Scheme = ""
		}

		// 填充一些必要信息
		request.RemoteAddr = hc.remoteAddr
		request.Close = shouldClose(request.ProtoMajor, request.ProtoMinor, request.Header, false)

		// 重置状态，为下一个请求做准备
		hc.parser.Reset()
		return request, nil
	}

	// 数据包不全，等待下一轮继续解析
	return nil, nil
}

func (hc *HttpConn) Handle(handler http.Handler) (err error) {

	// fallback to default server mux
	if nil == handler {
		handler = http.DefaultServeMux
	}

	hc.writer.protoMajor = byte(hc.request.ProtoMajor)
	hc.writer.protoMinor = byte(hc.request.ProtoMinor)

	// 处理请求
	handler.ServeHTTP(hc.writer, hc.request)

	// 丢弃未读取的body数据
	if nil != hc.request.Body && hc.request.Body != http.NoBody {
		_, _ = io.Copy(io.Discard, hc.request.Body)
		_ = hc.request.Body.Close()
	}

	// 使用bufio包装一下降低系统调用次数以提高性能
	bw := bufWriterPool.Get().(*bufio.Writer)
	bw.Reset(hc.conn)

	// 回写响应: 使用高性能版
	if err = hc.writer.writeFastResponse(hc.request, bw); nil == err {
		err = bw.Flush()
	}

	// 回写响应: 使用标准库版
	//if err = hc.writer.writeStdResponse(request, bw); nil == err {
	//	err = bw.Flush()
	//}

	// 用完回收
	bw.Reset(nil)
	bufWriterPool.Put(bw)

	return err
}

// Determine whether to hang up after sending a request and body, or
// receiving a response and body
// 'header' is the request headers.
func shouldClose(major, minor int, header http.Header, removeCloseHeader bool) bool {
	if major < 1 {
		return true
	}

	conv := header["Connection"]
	hasClose := httpguts.HeaderValuesContainsToken(conv, "close")
	if major == 1 && minor == 0 {
		return hasClose || !httpguts.HeaderValuesContainsToken(conv, "keep-alive")
	}

	if hasClose && removeCloseHeader {
		header.Del("Connection")
	}

	return hasClose
}
