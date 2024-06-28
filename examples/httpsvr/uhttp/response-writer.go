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
	"strconv"

	"github.com/urpc/uio"
)

type responseWriter interface {
	io.Writer
	io.ByteWriter
	io.StringWriter
}

type httpResponseWriter struct {
	protoMajor, protoMinor byte
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

	if "" == header.Get("Date") {
		var timeRFC1123 = NowRFC1123String()
		header.Set("Date", timeRFC1123)
	}

	resp := &http.Response{
		ProtoMajor:    int(h.protoMajor),
		ProtoMinor:    int(h.protoMinor),
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

		if err := w.WriteByte('0' + h.protoMajor); nil != err {
			return err
		}

		if err := w.WriteByte('.'); nil != err {
			return err
		}

		if err := w.WriteByte('0' + h.protoMinor); nil != err {
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

		if nil == h.header || h.header.Get("Date") == "" {
			if _, err := w.WriteString("Date: "); err != nil {
				return err
			}

			var timeRFC1123 = NowRFC1123String()
			if _, err := w.WriteString(timeRFC1123); err != nil {
				return err
			}

			if _, err := w.WriteString("\r\n"); err != nil {
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
