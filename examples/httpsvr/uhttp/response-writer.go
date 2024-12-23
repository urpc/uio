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
	"io"
	"net/http"
	"net/http/httputil"
	"strconv"
)

type responseWriter interface {
	io.Writer
	io.ByteWriter
	io.StringWriter
	Flush() error
}

type httpResponseWriter struct {
	protoMajor, protoMinor byte
	statusCode             int
	header                 http.Header
	headerWritten          bool
	request                *http.Request
	writer                 responseWriter
	chunkWriter            io.WriteCloser
}

func (h *httpResponseWriter) Header() http.Header {
	if nil == h.header {
		h.header = make(http.Header)
	}
	return h.header
}

func (h *httpResponseWriter) Write(b []byte) (int, error) {

	if !h.headerWritten {
		h.headerWritten = true
		if 0 == h.statusCode {
			h.statusCode = http.StatusOK
		}

		if chunked, err := h.writeResponseHeader(h.request, h.writer); nil != err {
			return 0, err
		} else if chunked {
			h.chunkWriter = httputil.NewChunkedWriter(h.writer)
		}
	}

	if nil != h.chunkWriter {
		return h.chunkWriter.Write(b)
	}

	return h.writer.Write(b)
}

func (h *httpResponseWriter) Close() error {
	if nil != h.chunkWriter {
		if err := h.chunkWriter.Close(); nil != err {
			return err
		}
		_, _ = h.writer.WriteString("\r\n")
	}

	return h.writer.Flush()
}

func (h *httpResponseWriter) WriteHeader(statusCode int) {
	h.statusCode = statusCode
}

func (h *httpResponseWriter) writeResponseHeader(request *http.Request, w responseWriter) (chunked bool, err error) {

	// Status line
	text := http.StatusText(h.statusCode)
	if text == "" {
		text = "status code " + strconv.Itoa(h.statusCode)
	}

	// HTTP/1.1 200 OK\r\n
	{
		if _, err = w.WriteString("HTTP/"); nil != err {
			return
		}

		if err = w.WriteByte('0' + h.protoMajor); nil != err {
			return
		}

		if err = w.WriteByte('.'); nil != err {
			return
		}

		if err = w.WriteByte('0' + h.protoMinor); nil != err {
			return
		}

		if err = w.WriteByte(' '); nil != err {
			return
		}

		if _, err = w.WriteString(strconv.Itoa(h.statusCode)); nil != err {
			return
		}

		if err = w.WriteByte(' '); nil != err {
			return
		}

		if _, err = w.WriteString(text); nil != err {
			return
		}

		if _, err = w.WriteString("\r\n"); nil != err {
			return
		}
	}

	// Header: Value\r\n
	{
		// Connection: close
		if value := request.Header.Get("Connection"); "close" == value || request.Close {
			if _, err = w.WriteString("Connection: close\r\n"); nil != err {
				return
			}
		}

		if nil == h.header || h.header.Get("Content-Type") == "" {
			if _, err = w.WriteString("Content-Type: text/plain; charset=utf-8\r\n"); err != nil {
				return
			}
		}

		if nil == h.header || h.header.Get("Date") == "" {
			if _, err = w.WriteString("Date: "); err != nil {
				return
			}

			var timeRFC1123 = NowRFC1123String()
			if _, err = w.WriteString(timeRFC1123); err != nil {
				return
			}

			if _, err = w.WriteString("\r\n"); err != nil {
				return
			}
		}

		if (nil == h.header || h.header.Get("Content-Length") == "") && h.header.Get("Transfer-Encoding") == "" {
			if _, err = w.WriteString("Transfer-Encoding: chunked\r\n"); err != nil {
				return
			}
			chunked = true
		}

		for key, values := range h.header {

			for _, value := range values {
				if _, err = w.WriteString(key); nil != err {
					return
				}

				if _, err = w.WriteString(": "); nil != err {
					return
				}

				if _, err = w.WriteString(value); nil != err {
					return
				}

				if _, err = w.WriteString("\r\n"); nil != err {
					return
				}
			}
		}
	}

	// End-of-header
	if _, err = w.WriteString("\r\n"); err != nil {
		return
	}

	return
}
