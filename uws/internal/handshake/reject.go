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

package handshake

import (
	"fmt"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
)

// MaxRejectBody bounds the body of a refused handshake that a client reads
// for its reason, and the reason a server sends.
const MaxRejectBody = 4 << 10

// ResponseError is a server's refusal of the upgrade: a complete response
// whose status is not 101. It wraps ErrNotUpgrade.
type ResponseError struct {
	StatusCode int
	Header     http.Header
	// Body is the response body when it came with a Content-Length of at most
	// MaxRejectBody, the reason a uws server gives.
	Body []byte
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("websocket: handshake refused with status %d", e.StatusCode)
}

func (e *ResponseError) Unwrap() error { return ErrNotUpgrade }

// reservedRequestHeaders are the headers the upgrade itself is made of, which
// a caller may not set.
var reservedRequestHeaders = map[string]bool{
	"Host":                     true,
	"Upgrade":                  true,
	"Connection":               true,
	"Content-Length":           true,
	"Transfer-Encoding":        true,
	"Expect":                   true,
	"Sec-Websocket-Key":        true,
	"Sec-Websocket-Version":    true,
	"Sec-Websocket-Protocol":   true,
	"Sec-Websocket-Extensions": true,
	"Sec-Websocket-Accept":     true,
}

// reservedRejectHeaders are the headers a refusal's framing is made of.
var reservedRejectHeaders = map[string]bool{
	"Connection":        true,
	"Content-Length":    true,
	"Transfer-Encoding": true,
}

// CheckRequestHeader reports whether h may be added to an upgrade request:
// valid names and values, none of them a header the upgrade sets itself.
func CheckRequestHeader(h http.Header) error {
	return checkHeader(h, reservedRequestHeaders)
}

// CheckRejectHeader reports whether h may be added to a refusal.
func CheckRejectHeader(h http.Header) error {
	return checkHeader(h, reservedRejectHeaders)
}

func checkHeader(h http.Header, reserved map[string]bool) error {
	for name, values := range h {
		if !validToken(name) {
			return fmt.Errorf("invalid header name %q", name)
		}
		if reserved[textproto.CanonicalMIMEHeaderKey(name)] {
			return fmt.Errorf("header %s is set by the handshake", name)
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n\x00") {
				return fmt.Errorf("invalid value for header %s", name)
			}
		}
	}
	return nil
}

// writeHeader writes h in a stable order, one line per value.
func writeHeader(b *strings.Builder, h http.Header) {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		key := textproto.CanonicalMIMEHeaderKey(name)
		for _, value := range h[name] {
			b.WriteString(key)
			b.WriteString(": ")
			b.WriteString(value)
			b.WriteString("\r\n")
		}
	}
}

// BuildRejectResponse returns a complete refusal of an upgrade: status,
// header and reason as a plain-text body, the connection to be closed after.
// The reason is cut to MaxRejectBody bytes; header must pass
// CheckRejectHeader.
func BuildRejectResponse(status int, header http.Header, reason string) []byte {
	text := http.StatusText(status)
	if text == "" {
		text = "Status"
	}
	if len(reason) > MaxRejectBody {
		reason = reason[:MaxRejectBody]
	}
	var b strings.Builder
	b.Grow(128 + len(reason))
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(status))
	b.WriteByte(' ')
	b.WriteString(text)
	b.WriteString("\r\nConnection: close\r\n")
	if header.Get("Content-Type") == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	}
	b.WriteString("Content-Length: ")
	b.WriteString(strconv.Itoa(len(reason)))
	b.WriteString("\r\n")
	writeHeader(&b, header)
	b.WriteString("\r\n")
	b.WriteString(reason)
	return []byte(b.String())
}
