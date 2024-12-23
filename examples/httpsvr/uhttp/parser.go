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
	"net/url"
	"strings"

	"github.com/antlabs/httparser"
	"github.com/urpc/uio"
	"golang.org/x/net/http/httpguts"
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
	writer.headerWritten = false
	clear(writer.header)
	return writer
}

var httpParserSettings = &httparser.Setting{
	MessageBegin: func(p *httparser.Parser, _ int) {
		//fmt.Printf("begin\n")
		hConn := p.GetUserData().(*HttpConn)
		hConn.request = resetHttpRequest(hConn.request)
		hConn.writer = resetHttpResponseWriter(hConn.writer)
	},
	URL: func(p *httparser.Parser, buf []byte, _ int) {
		//fmt.Printf("url->%s\n", buf)
		hConn := p.GetUserData().(*HttpConn)
		hConn.request.RequestURI = string(buf)
	},
	Status: func(p *httparser.Parser, buf []byte, _ int) {
		// response only
	},
	HeaderField: func(p *httparser.Parser, buf []byte, _ int) {
		// http header field
		// fmt.Printf("header field:%s\n", buf)
		hConn := p.GetUserData().(*HttpConn)
		hConn.lastHeader = string(buf)
	},
	HeaderValue: func(p *httparser.Parser, buf []byte, _ int) {
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
	HeadersComplete: func(p *httparser.Parser, _ int) {
		//fmt.Printf("header complete\n")
	},
	Body: func(p *httparser.Parser, buf []byte, _ int) {
		//fmt.Printf("%s", buf)
		// Content-Length or chunked data
		var bodyBuffer uio.CompositeBuffer
		bodyBuffer.Write(buf)

		hConn := p.GetUserData().(*HttpConn)
		hConn.request.Body = &bodyBuffer
	},
	MessageComplete: func(p *httparser.Parser, _ int) {
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

		// CONNECT requests are used two different ways, and neither uses a full URL:
		// The standard use is to tunnel HTTPS through an HTTP proxy.
		// It looks like "CONNECT www.google.com:443 HTTP/1.1", and the parameter is
		// just the authority section of a URL. This information should go in req.URL.Host.
		//
		// The net/rpc package also uses CONNECT, but there the parameter is a path
		// that starts with a slash. It can be parsed with the regular URL parser,
		// and the path will end up in req.URL.Path, where it needs to be in order for
		// RPC to work.
		rawurl := hConn.request.RequestURI
		justAuthority := hConn.request.Method == "CONNECT" && !strings.HasPrefix(rawurl, "/")
		if justAuthority {
			rawurl = "http://" + rawurl
		}

		hConn.request.URL, _ = url.ParseRequestURI(rawurl)

		if justAuthority {
			// Strip the bogus "http://" back off.
			hConn.request.URL.Scheme = ""
		}

		// fill remote addr and close flag.
		hConn.request.RemoteAddr = hConn.remoteAddr
		hConn.request.Close = shouldClose(hConn.request.ProtoMajor, hConn.request.ProtoMinor, hConn.request.Header, false)
		hConn.Handle()
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
