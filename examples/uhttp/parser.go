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
	"net/http"
	"strings"

	"github.com/antlabs/httparser"
	"github.com/urpc/uio"
)

var emptyRequest = http.Request{}

func resetHttpRequest(req *http.Request) *http.Request {
	var header = req.Header
	clear(header)

	*req = emptyRequest
	req.Header = header
	req.Body = http.NoBody
	return req
}

func resetHttpResponseWriter(writer *httpResponseWriter) *httpResponseWriter {
	writer.protoMajor = 0
	writer.protoMinor = 0
	writer.statusCode = 0
	writer.body.Reset()
	clear(writer.header)
	return writer
}

var httpParserSettings = &httparser.Setting{
	MessageBegin: func(p *httparser.Parser) {
		//解析器开始工作
		//fmt.Printf("begin\n")
		hConn := p.GetUserData().(*HttpConn)
		hConn.request = resetHttpRequest(hConn.request)
		hConn.writer = resetHttpResponseWriter(hConn.writer)
		hConn.finished = false
	},
	URL: func(p *httparser.Parser, buf []byte) {
		//url数据
		//fmt.Printf("url->%s\n", buf)
		hConn := p.GetUserData().(*HttpConn)
		hConn.request.RequestURI = string(buf)
	},
	Status: func(p *httparser.Parser, buf []byte) {
		// 响应包才需要用到
	},
	HeaderField: func(p *httparser.Parser, buf []byte) {
		// http header field
		// fmt.Printf("header field:%s\n", buf)
		hConn := p.GetUserData().(*HttpConn)
		hConn.lastHeader = string(buf)
	},
	HeaderValue: func(p *httparser.Parser, buf []byte) {
		// http header value
		//fmt.Printf("header value:%s\n", buf)
		hConn := p.GetUserData().(*HttpConn)
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

		hConn := p.GetUserData().(*HttpConn)
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

		hConn := p.GetUserData().(*HttpConn)
		hConn.request.Method = getMethod(p.Method)
		hConn.request.Proto = sb.String()
		hConn.request.ProtoMajor = int(p.Major)
		hConn.request.ProtoMinor = int(p.Minor)
		hConn.finished = true
	},
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
