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

package main

import (
	"bufio"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/antlabs/httparser"
	"github.com/urpc/uio"
	"golang.org/x/net/http/httpguts"
)

var emptyRequest = http.Request{}

var bufWriterPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewWriter(nil)
	},
}

type httpConn struct {
	parser     *httparser.Parser
	setting    *httparser.Setting
	buffer     []byte
	request    *http.Request
	lastHeader string
	finished   bool
}

func newHttpConn() *httpConn {

	hConn := &httpConn{
		parser:  httparser.New(httparser.REQUEST),
		buffer:  make([]byte, 1024),
		request: &http.Request{},
	}

	hConn.setting = &httparser.Setting{
		MessageBegin: func(p *httparser.Parser) {
			//解析器开始工作
			//fmt.Printf("begin\n")
			*hConn.request = emptyRequest
			hConn.request.Header = make(http.Header, 8)
			hConn.request.Body = http.NoBody
		},
		URL: func(p *httparser.Parser, buf []byte) {
			//url数据
			//fmt.Printf("url->%s\n", buf)
			hConn.request.RequestURI = string(buf)
		},
		Status: func(p *httparser.Parser, buf []byte) {
			// 响应包才需要用到
		},
		HeaderField: func(p *httparser.Parser, buf []byte) {
			// http header field
			// fmt.Printf("header field:%s\n", buf)
			hConn.lastHeader = string(buf)
		},
		HeaderValue: func(p *httparser.Parser, buf []byte) {
			// http header value
			//fmt.Printf("header value:%s\n", buf)
			if "Host" == hConn.lastHeader {
				hConn.request.Host = string(buf)
			} else {
				hConn.request.Header.Add(hConn.lastHeader, string(buf))
			}
			hConn.lastHeader = ""
		},
		HeadersComplete: func(p *httparser.Parser) {
			// http header解析结束
			//fmt.Printf("header complete\n")
		},
		Body: func(p *httparser.Parser, buf []byte) {
			//fmt.Printf("%s", buf)
			// Content-Length 或者chunked数据包
			var bodyBuffer uio.CompositeBuffer
			bodyBuffer.Write(buf)
			hConn.request.Body = &bodyBuffer
		},
		MessageComplete: func(p *httparser.Parser) {
			// 消息解析结束
			//fmt.Printf("\n")
			var sb strings.Builder
			sb.WriteString("HTTP/")
			sb.WriteByte('0' + p.Major)
			sb.WriteByte('.')
			sb.WriteByte('0' + p.Minor)

			hConn.request.Method = getMethod(p.Method)
			hConn.request.Proto = sb.String()
			hConn.request.ProtoMajor = int(p.Major)
			hConn.request.ProtoMinor = int(p.Minor)
			hConn.finished = true
		},
	}

	return hConn
}

func (hc *httpConn) ServeHTTP(c uio.Conn, handler http.Handler) error {

	request, err := hc.parseHttpRequest(c)
	if nil != err {
		return err
	}

	if nil != request {
		// fallback to default server mux
		if nil == handler {
			handler = http.DefaultServeMux
		}
		return hc.handleRequest(c, request, handler)
	}

	return nil
}

func (hc *httpConn) parseHttpRequest(c uio.Conn) (r *http.Request, err error) {

	// 获取当前已接受但是未处理的数据
	buffer := c.Peek(hc.buffer)

	// 解析http数据
	parsedBytes, err := hc.parser.Execute(hc.setting, buffer)
	if nil != err {
		return nil, err
	}

	// 成功解析部分或者全部数据
	if parsedBytes > 0 {
		_, _ = c.Discard(parsedBytes)
	}

	// 成功解析一个请求
	if hc.finished {
		// 重置状态，为下一个请求做准备
		hc.parser.Reset()
		hc.finished = false
		return hc.request, nil
	}

	// 数据包不全，等待下一轮继续解析
	return nil, nil
}

func (hc *httpConn) handleRequest(c uio.Conn, request *http.Request, handler http.Handler) (err error) {

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
		return err
	}

	if justAuthority {
		// Strip the bogus "http://" back off.
		request.URL.Scheme = ""
	}

	request.RemoteAddr = c.RemoteAddr().String()
	request.Close = shouldClose(request.ProtoMajor, request.ProtoMinor, request.Header, false)

	// 准备响应收集容器
	writer := &httpResponseWriter{
		protoMajor: request.ProtoMajor,
		protoMinor: request.ProtoMinor,
	}

	// 处理请求
	handler.ServeHTTP(writer, request)

	// 丢弃未读取的body数据
	if nil != request.Body && request.Body != http.NoBody {
		_, _ = io.Copy(io.Discard, request.Body)
		_ = request.Body.Close()
	}

	// 使用bufio包装一下降低系统调用次数以提高性能
	bw := bufWriterPool.Get().(*bufio.Writer)
	bw.Reset(c)

	// 回写响应: 使用高性能版
	if err = writer.writeFastResponse(request, bw); nil == err {
		err = bw.Flush()
	}

	// 回写响应: 使用标准库版
	//if err = writer.writeStdResponse(request, bw); nil == err {
	//	err = bw.Flush()
	//}

	// 用完回收
	bw.Reset(nil)
	bufWriterPool.Put(bw)

	return err
}

type responseWriter interface {
	io.Writer
	io.ByteWriter
	io.StringWriter
}

type httpResponseWriter struct {
	protoMajor, protoMinor int
	statusCode             int
	header                 http.Header
	body                   uio.CompositeBuffer
}

func (h *httpResponseWriter) Header() http.Header {
	if nil == h.header {
		h.header = make(http.Header)
	}
	return h.header
}

func (h *httpResponseWriter) Write(b []byte) (int, error) {
	return h.body.Write(b)
}

func (h *httpResponseWriter) WriteHeader(statusCode int) {
	h.statusCode = statusCode
}

func (h *httpResponseWriter) writeStdResponse(request *http.Request, w responseWriter) error {
	if 0 == h.statusCode {
		h.WriteHeader(http.StatusOK)
	}

	var header = h.Header()
	if "" == header.Get("Content-Type") {
		header.Set("Content-Type", "text/plain; charset=utf-8")
	}

	if "" == h.header.Get("Server") {
		header.Set("Server", "uio")
	}

	resp := &http.Response{
		ProtoMajor:    h.protoMajor,
		ProtoMinor:    h.protoMinor,
		StatusCode:    h.statusCode,
		Header:        header,
		Body:          &h.body,
		ContentLength: int64(h.body.Len()),
		Request:       request,
	}

	return resp.Write(w)
}

func (h *httpResponseWriter) writeFastResponse(request *http.Request, w responseWriter) error {

	if 0 == h.statusCode {
		h.WriteHeader(http.StatusOK)
	}

	var contentLength = h.body.Len()

	// Status line
	text := http.StatusText(h.statusCode)
	if text == "" {
		text = "status code " + strconv.Itoa(h.statusCode)
	}

	// HTTP/1.1 200 OK\r\n
	{
		if _, err := w.WriteString("HTTP/"); nil != err {
			return err
		}

		if err := w.WriteByte('0' + byte(h.protoMajor)); nil != err {
			return err
		}

		if err := w.WriteByte('.'); nil != err {
			return err
		}

		if err := w.WriteByte('0' + byte(h.protoMinor)); nil != err {
			return err
		}

		if err := w.WriteByte(' '); nil != err {
			return err
		}

		if _, err := w.WriteString(strconv.Itoa(h.statusCode)); nil != err {
			return err
		}

		if err := w.WriteByte(' '); nil != err {
			return err
		}

		if _, err := w.WriteString(text); nil != err {
			return err
		}

		if _, err := w.WriteString("\r\n"); nil != err {
			return err
		}
	}

	// Header: Value\r\n
	{
		// Connection: close
		if value := request.Header.Get("Connection"); "close" == value || request.Close {
			if _, err := w.WriteString("Connection: close\r\n"); nil != err {
				return err
			}
		}

		// Content-Length: 1024
		if contentLength > 0 {
			if _, err := w.WriteString("Content-Length: "); err != nil {
				return err
			}

			if _, err := w.WriteString(strconv.FormatInt(int64(contentLength), 10)); err != nil {
				return err
			}

			if _, err := w.WriteString("\r\n"); nil != err {
				return err
			}
		}

		if nil == h.header || h.header.Get("Content-Type") == "" {
			if _, err := w.WriteString("Content-Type: text/plain; charset=utf-8\r\n"); err != nil {
				return err
			}
		}

		if nil == h.header || h.header.Get("Server") == "" {
			if _, err := w.WriteString("Server: uio\r\n"); err != nil {
				return err
			}
		}

		for key, values := range h.header {

			for _, value := range values {
				if _, err := w.WriteString(key); nil != err {
					return err
				}

				if _, err := w.WriteString(": "); nil != err {
					return err
				}

				if _, err := w.WriteString(value); nil != err {
					return err
				}

				if _, err := w.WriteString("\r\n"); nil != err {
					return err
				}
			}
		}
	}

	// End-of-header
	if _, err := w.WriteString("\r\n"); err != nil {
		return err
	}

	// write body
	if contentLength > 0 {
		if _, err := h.body.WriteTo(w); nil != err {
			return err
		}
		return h.body.Close()
	}

	return nil
}

var methods = map[httparser.Method]string{
	httparser.GET:     "GET",
	httparser.HEAD:    "HEAD",
	httparser.POST:    "POST",
	httparser.PUT:     "PUT",
	httparser.DELETE:  "DELETE",
	httparser.CONNECT: "CONNECT",
	httparser.OPTIONS: "OPTIONS",
	httparser.TRACE:   "TRACE",
}

func getMethod(m httparser.Method) string {
	return methods[m]
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
