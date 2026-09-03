package handshake

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	webSocketGUID   = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	DefaultMaxBytes = 32 << 10
)

var (
	ErrBadRequest = errors.New("websocket: invalid handshake")
	ErrNotUpgrade = errors.New("websocket: not an upgrade request")
)

type ServerOptions struct {
	MaxHeaderBytes int
	CheckOrigin    func(*http.Request) bool
	Subprotocols   []string
}

type Request struct {
	HTTP         *http.Request
	Key          string
	Subprotocols []string
	Extensions   []string
}

func ParseServerRequest(data []byte, options ServerOptions) (Request, int, error) {
	maxBytes := options.MaxHeaderBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	end := bytes.Index(data, []byte("\r\n\r\n"))
	if end < 0 {
		if len(data) > maxBytes {
			return Request{}, 0, ErrBadRequest
		}
		return Request{}, 0, io.ErrUnexpectedEOF
	}
	end += 4
	if end > maxBytes {
		return Request{}, 0, ErrBadRequest
	}
	reader := newHeaderReader(data[:end])
	req, err := http.ReadRequest(reader)
	if err != nil {
		return Request{}, 0, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	request, err := ValidateServerRequest(req, options)
	if err != nil {
		return Request{}, 0, err
	}
	return request, end, nil
}

// ValidateServerRequest validates an HTTP/1.1 WebSocket upgrade request that
// has already been parsed by net/http.
func ValidateServerRequest(req *http.Request, options ServerOptions) (Request, error) {
	if req == nil {
		return Request{}, ErrBadRequest
	}
	if req.Method != http.MethodGet || req.ProtoMajor != 1 || req.ProtoMinor != 1 {
		return Request{}, ErrNotUpgrade
	}
	if !headerHasToken(req.Header, "Upgrade", "websocket") ||
		!headerHasToken(req.Header, "Connection", "upgrade") {
		return Request{}, ErrNotUpgrade
	}
	if values := req.Header.Values("Sec-WebSocket-Version"); len(values) != 1 || strings.TrimSpace(values[0]) != "13" {
		return Request{}, ErrNotUpgrade
	}
	keys := req.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 || !validKey(keys[0]) {
		return Request{}, ErrBadRequest
	}
	if req.ContentLength != 0 || len(req.TransferEncoding) != 0 || req.Header.Get("Expect") != "" {
		return Request{}, ErrBadRequest
	}
	if options.CheckOrigin != nil && !options.CheckOrigin(req) {
		return Request{}, ErrNotUpgrade
	}
	protocols, err := parseTokens(req.Header.Values("Sec-WebSocket-Protocol"))
	if err != nil {
		return Request{}, err
	}
	return Request{
		HTTP:         req,
		Key:          strings.TrimSpace(keys[0]),
		Subprotocols: protocols,
		Extensions:   req.Header.Values("Sec-WebSocket-Extensions"),
	}, nil
}

func BuildServerResponse(req Request, selectedSubprotocol string, extensions string) []byte {
	return AppendServerResponse(make([]byte, 0, ServerResponseSize(selectedSubprotocol, extensions)), req, selectedSubprotocol, extensions)
}

const sha1Base64Size = 28

const baseServerResponseSize = len("HTTP/1.1 101 Switching Protocols\r\n") +
	len("Upgrade: websocket\r\n") +
	len("Connection: Upgrade\r\n") +
	len("Sec-WebSocket-Accept: ") + sha1Base64Size +
	len("\r\n") + len("\r\n")

// ServerResponseSize returns the exact number of bytes appended by
// AppendServerResponse.
func ServerResponseSize(selectedSubprotocol, extensions string) int {
	size := baseServerResponseSize
	if selectedSubprotocol != "" {
		size += len("Sec-WebSocket-Protocol: ") + len(selectedSubprotocol) + len("\r\n")
	}
	if extensions != "" {
		size += len("Sec-WebSocket-Extensions: ") + len(extensions) + len("\r\n")
	}
	return size
}

// AppendServerResponse appends an RFC 6455 upgrade response to dst.
func AppendServerResponse(dst []byte, req Request, selectedSubprotocol, extensions string) []byte {
	dst = append(dst, "HTTP/1.1 101 Switching Protocols\r\n"...)
	dst = append(dst, "Upgrade: websocket\r\n"...)
	dst = append(dst, "Connection: Upgrade\r\n"...)
	dst = append(dst, "Sec-WebSocket-Accept: "...)
	dst = appendAcceptKey(dst, req.Key)
	dst = append(dst, "\r\n"...)
	if selectedSubprotocol != "" {
		dst = append(dst, "Sec-WebSocket-Protocol: "...)
		dst = append(dst, selectedSubprotocol...)
		dst = append(dst, "\r\n"...)
	}
	if extensions != "" {
		dst = append(dst, "Sec-WebSocket-Extensions: "...)
		dst = append(dst, extensions...)
		dst = append(dst, "\r\n"...)
	}
	return append(dst, "\r\n"...)
}

func SelectSubprotocol(offered, supported []string) string {
	for _, want := range supported {
		if !validToken(want) {
			continue
		}
		for _, got := range offered {
			if want == got {
				return want
			}
		}
	}
	return ""
}

func BuildClientRequest(target *url.URL, key string, protocols []string, extensions string) ([]byte, error) {
	if target == nil || target.Host == "" || (target.Scheme != "ws" && target.Scheme != "wss") {
		return nil, ErrBadRequest
	}
	if !validKey(key) {
		return nil, ErrBadRequest
	}
	for _, protocol := range protocols {
		if !validToken(protocol) {
			return nil, ErrBadRequest
		}
	}
	path := target.EscapedPath()
	if path == "" {
		path = "/"
	}
	if target.RawQuery != "" {
		path += "?" + target.RawQuery
	}
	var b strings.Builder
	b.Grow(256)
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&b, "Host: %s\r\n", target.Host)
	b.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: ")
	b.WriteString(key)
	b.WriteString("\r\n")
	if len(protocols) > 0 {
		b.WriteString("Sec-WebSocket-Protocol: ")
		b.WriteString(strings.Join(protocols, ", "))
		b.WriteString("\r\n")
	}
	if extensions != "" {
		b.WriteString("Sec-WebSocket-Extensions: ")
		b.WriteString(extensions)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	return []byte(b.String()), nil
}

// ValidateClientResponse parses and validates a server handshake response and
// returns its consumed bytes, selected subprotocol, and extension headers.
func ValidateClientResponse(data []byte, key string, protocols []string, limits ...int) (int, string, []string, error) {
	maxBytes := responseLimit(limits)
	end := bytes.Index(data, []byte("\r\n\r\n"))
	if end < 0 {
		if len(data) > maxBytes {
			return 0, "", nil, ErrBadRequest
		}
		return 0, "", nil, io.ErrUnexpectedEOF
	}
	end += 4
	if end > maxBytes {
		return 0, "", nil, ErrBadRequest
	}
	reader := newHeaderReader(data[:end])
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		return 0, "", nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	acceptValues := resp.Header.Values("Sec-WebSocket-Accept")
	if len(acceptValues) != 1 ||
		resp.StatusCode != http.StatusSwitchingProtocols ||
		!headerHasToken(resp.Header, "Upgrade", "websocket") ||
		!headerHasToken(resp.Header, "Connection", "upgrade") ||
		strings.TrimSpace(acceptValues[0]) != acceptKey(key) {
		return 0, "", nil, ErrNotUpgrade
	}
	protocolValues := resp.Header.Values("Sec-WebSocket-Protocol")
	if len(protocolValues) > 1 {
		return 0, "", nil, ErrNotUpgrade
	}
	selected := ""
	if len(protocolValues) == 1 {
		selected = strings.TrimSpace(protocolValues[0])
	}
	if selected != "" && !contains(protocols, selected) {
		return 0, "", nil, ErrNotUpgrade
	}
	return end, selected, resp.Header.Values("Sec-WebSocket-Extensions"), nil
}

func responseLimit(limits []int) int {
	if len(limits) > 0 && limits[0] > 0 {
		return limits[0]
	}
	return DefaultMaxBytes
}

func newHeaderReader(data []byte) *bufio.Reader {
	return bufio.NewReaderSize(bytes.NewReader(data), len(data))
}

func acceptKey(key string) string {
	var encoded [sha1Base64Size]byte
	result := appendAcceptKey(encoded[:0], key)
	return string(result)
}

func appendAcceptKey(dst []byte, key string) []byte {
	var challenge [64]byte
	if len(key)+len(webSocketGUID) > len(challenge) {
		h := sha1.New()
		_, _ = io.WriteString(h, key)
		_, _ = io.WriteString(h, webSocketGUID)
		return appendBase64SHA1(dst, h.Sum(nil))
	}
	n := copy(challenge[:], key)
	n += copy(challenge[n:], webSocketGUID)
	sum := sha1.Sum(challenge[:n])
	return appendBase64SHA1(dst, sum[:])
}

func appendBase64SHA1(dst, sum []byte) []byte {
	var encoded [sha1Base64Size]byte
	base64.StdEncoding.Encode(encoded[:], sum)
	return append(dst, encoded[:]...)
}

func validKey(key string) bool {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	return err == nil && len(decoded) == 16
}

func parseTokens(values []string) ([]string, error) {
	var tokens []string
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				if !validToken(token) {
					return nil, ErrBadRequest
				}
				tokens = append(tokens, token)
			}
		}
	}
	return tokens, nil
}

func validToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c <= 0x20 || c >= 0x7f {
			return false
		}
		switch c {
		case '(', ')', '<', '>', '@', ',', ';', ':', '\\', '"', '/', '[', ']', '?', '=', '{', '}':
			return false
		}
	}
	return true
}

func headerHasToken(header http.Header, name, want string) bool {
	for _, value := range header.Values(name) {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
