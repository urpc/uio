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

	// peek unread bytes from inbound buffer.
	buffer := hc.conn.Peek(hc.buffer)

	// parse http request.
	parsedBytes, err := hc.parser.Execute(httpParserSettings, buffer)
	if nil != err {
		return nil, err
	}

	// advance parsed bytes offset.
	if parsedBytes > 0 {
		_, _ = hc.conn.Discard(parsedBytes)
	}

	// finished parse http request.
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

		// fill remote addr and close flag.
		request.RemoteAddr = hc.remoteAddr
		request.Close = shouldClose(request.ProtoMajor, request.ProtoMinor, request.Header, false)

		// reset parser state for next request.
		hc.parser.Reset()
		return request, nil
	}

	// wait for more bytes to continue.
	return nil, nil
}

func (hc *HttpConn) Handle(handler http.Handler) (err error) {

	// fallback to default server mux
	if nil == handler {
		handler = http.DefaultServeMux
	}

	hc.writer.protoMajor = byte(hc.request.ProtoMajor)
	hc.writer.protoMinor = byte(hc.request.ProtoMinor)

	// handle request.
	handler.ServeHTTP(hc.writer, hc.request)

	// drop any unread http body data.
	if nil != hc.request.Body && hc.request.Body != http.NoBody {
		_, _ = io.Copy(io.Discard, hc.request.Body)
		_ = hc.request.Body.Close()
	}

	// merge multi write to one.
	bw := bufWriterPool.Get().(*bufio.Writer)
	bw.Reset(hc.conn)

	// write response: fast version.
	if err = hc.writer.writeFastResponse(hc.request, bw); nil == err {
		err = bw.Flush()
	}

	// write response: std version.
	//if err = hc.writer.writeStdResponse(request, bw); nil == err {
	//	err = bw.Flush()
	//}

	// reset and put to buffer pool.
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
