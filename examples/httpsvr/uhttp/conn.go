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
	"sync"

	"github.com/antlabs/httparser"
	"github.com/urpc/uio"
)

var bufWriterPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewWriterSize(nil, 1024)
	},
}

type HttpConn struct {
	conn       uio.Conn
	handler    http.Handler
	remoteAddr string
	parser     *httparser.Parser
	buffer     []byte
	request    *http.Request
	writer     *httpResponseWriter
	lastHeader string
}

func NewHttpConn(conn uio.Conn, handler http.Handler) *HttpConn {

	// fallback to default server mux
	if nil == handler {
		handler = http.DefaultServeMux
	}

	hConn := &HttpConn{
		conn:       conn,
		handler:    handler,
		remoteAddr: conn.RemoteAddr().String(),
		parser:     httparser.New(httparser.REQUEST),
		buffer:     make([]byte, 2048),
		request:    &http.Request{Header: make(http.Header), Body: http.NoBody},
		writer:     &httpResponseWriter{},
	}
	hConn.parser.SetUserData(hConn)
	return hConn
}

func (hc *HttpConn) ServeHTTP() error {

	for {
		// peek unread bytes from inbound buffer.
		buffer := hc.conn.Peek(hc.buffer)

		// parse http request.
		parsedBytes, err := hc.parser.Execute(httpParserSettings, buffer)
		if nil != err {
			return err
		}

		// advance parsed bytes offset.
		if parsedBytes > 0 {
			_, _ = hc.conn.Discard(parsedBytes)
		} else {
			break
		}
	}

	return nil
}

func (hc *HttpConn) Handle() (err error) {

	// merge multi write to one.
	bw := bufWriterPool.Get().(*bufio.Writer)
	bw.Reset(hc.conn)

	hc.writer.protoMajor = byte(hc.request.ProtoMajor)
	hc.writer.protoMinor = byte(hc.request.ProtoMinor)
	hc.writer.request = hc.request
	hc.writer.writer = bw

	// handle request.
	hc.handler.ServeHTTP(hc.writer, hc.request)

	// drop any unread http body data.
	if nil != hc.request.Body && hc.request.Body != http.NoBody {
		_, _ = io.Copy(io.Discard, hc.request.Body)
		_ = hc.request.Body.Close()
	}

	// close writer
	err = hc.writer.Close()

	// reset and put to buffer pool.
	bw.Reset(nil)
	bufWriterPool.Put(bw)

	return err
}
