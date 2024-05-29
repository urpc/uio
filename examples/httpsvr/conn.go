package main

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/antlabs/httparser"
	"github.com/urpc/uio"
	"golang.org/x/net/http/httpguts"
)

var emptyRequest = http.Request{}

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
			var bodyBuffer bytes.Buffer
			bodyBuffer.Write(buf)
			hConn.request.Body = io.NopCloser(&bodyBuffer)
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
		return hc.handleRequest(c, request, handler)
	}

	return nil
}
func (hc *httpConn) parseHttpRequest(c uio.Conn) (r *http.Request, err error) {
	// 解析http数据
	parsedBytes, err := hc.parser.Execute(hc.setting, c.Peek(hc.buffer))
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

	// 回写结果
	writer := &httpResponseWriter{
		protoMajor: request.ProtoMajor,
		protoMinor: request.ProtoMinor,
	}

	// 回收缓存
	defer writer.body.Reset()

	// 处理请求
	handler.ServeHTTP(writer, request)

	// 丢弃未读取的body数据
	if nil != request.Body {
		_, _ = io.Copy(io.Discard, request.Body)
		_ = request.Body.Close()
	}

	// 准备响应
	response := writer.response()
	response.Request = request

	// 直接写数据到连接
	// wrk测试结果:  CPU 占用 ~3%
	//	$./wrk -t 1 -c 1 -d 10 --latency http://127.0.0.1:8080/
	//		Running 10s test @ http://127.0.0.1:8080/
	//		  1 threads and 1 connections
	//		  Thread Stats   Avg      Stdev     Max   +/- Stdev
	//		    Latency    49.78ms    3.53ms  60.00ms   98.51%
	//		    Req/Sec    20.00      1.41    30.00     98.02%
	//		  Latency Distribution
	//		     50%   50.00ms
	//		     75%   50.01ms
	//		     90%   50.04ms
	//		     99%   50.23ms
	//		  202 requests in 10.10s, 20.73KB read
	//		Requests/sec:     20.00
	//		Transfer/sec:      2.05KB
	//err = response.Write(c)

	// 使用bufio包装一下发送
	// wrk测试结果: CPU 占用 ~14%
	//
	//  $ ./wrk -t 1 -c 1 -d 10 --latency http://127.0.0.1:8080/
	//     Running 10s test @ http://127.0.0.1:8080/
	//     1 threads and 1 connections
	//     Thread Stats   Avg      Stdev     Max   +/- Stdev
	//     Latency    37.45us   35.29us   1.50ms   95.53%
	//     	Req/Sec    29.85k     1.24k   32.04k    74.26%
	//     	Latency Distribution
	//     50%   28.00us
	//     75%   35.00us
	//     90%   46.00us
	//     99%  219.00us
	//     299923 requests in 10.10s, 30.03MB read
	//     Requests/sec:  29696.81
	//     Transfer/sec:      2.97MB

	bw := bufio.NewWriter(c)
	if err = response.Write(bw); nil == err {
		err = bw.Flush()
	}

	return err
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

func (h *httpResponseWriter) response() *http.Response {
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

	return &http.Response{
		ProtoMajor:    h.protoMajor,
		ProtoMinor:    h.protoMinor,
		StatusCode:    h.statusCode,
		Header:        header,
		Body:          &h.body,
		ContentLength: int64(h.body.Len()),
	}
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
