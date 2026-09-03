package handshake

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const testKey = "dGhlIHNhbXBsZSBub25jZQ=="

func TestParseServerRequestAndResponse(t *testing.T) {
	data := []byte("GET /chat?room=1 HTTP/1.1\r\n" +
		"Host: example.test\r\n" +
		"Connection: keep-alive, Upgrade\r\n" +
		"Upgrade: WebSocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + testKey + "\r\n" +
		"Sec-WebSocket-Protocol: chat, superchat\r\n\r\n" +
		"\x81\x00")
	req, consumed, err := ParseServerRequest(data, ServerOptions{Subprotocols: []string{"superchat"}})
	if err != nil {
		t.Fatal(err)
	}
	if consumed != len(data)-2 || req.HTTP.Method != http.MethodGet || req.Key != testKey {
		t.Fatalf("request = consumed:%d method:%s key:%q", consumed, req.HTTP.Method, req.Key)
	}
	if got := SelectSubprotocol(req.Subprotocols, []string{"superchat"}); got != "superchat" {
		t.Fatalf("selected protocol = %q", got)
	}
	if got := SelectSubprotocol(req.Subprotocols, []string{"bad\r\nInjected: value", "superchat"}); got != "superchat" {
		t.Fatalf("invalid configured protocol was selected: %q", got)
	}
	response := BuildServerResponse(req, "superchat", "permessage-deflate")
	if len(response) != ServerResponseSize("superchat", "permessage-deflate") {
		t.Fatalf("response size = %d, want %d", len(response), ServerResponseSize("superchat", "permessage-deflate"))
	}
	if !bytes.Contains(response, []byte("Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=")) {
		t.Fatalf("response has wrong accept key: %s", response)
	}
	if !bytes.Contains(response, []byte("Sec-WebSocket-Protocol: superchat\r\n")) ||
		!bytes.Contains(response, []byte("Sec-WebSocket-Extensions: permessage-deflate\r\n")) {
		t.Fatalf("response missing negotiated headers: %s", response)
	}
}

func TestValidateServerRequestRejectsHTTPBody(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "http://example.test/ws", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	request.ProtoMajor = 1
	request.ProtoMinor = 1
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", testKey)
	if _, err = ValidateServerRequest(request, ServerOptions{}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("ValidateServerRequest() error = %v, want %v", err, ErrBadRequest)
	}
	request.Body = http.NoBody
	request.ContentLength = 0
	if parsed, err := ValidateServerRequest(request, ServerOptions{}); err != nil || parsed.Key != testKey {
		t.Fatalf("ValidateServerRequest() = %+v, %v", parsed, err)
	}
}

func TestParseServerRequestRejectsInvalidUpgrade(t *testing.T) {
	base := "GET / HTTP/1.1\r\nHost: example.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
	for _, test := range []struct {
		name string
		data string
		want error
	}{
		{name: "method", data: strings.Replace(base, "GET /", "POST /", 1), want: ErrNotUpgrade},
		{name: "version", data: strings.Replace(base, "13\r\n", "12\r\n", 1), want: ErrNotUpgrade},
		{name: "missing upgrade", data: strings.Replace(base, "Upgrade: websocket\r\n", "", 1), want: ErrNotUpgrade},
		{name: "bad key", data: strings.Replace(base, testKey, "bad", 1), want: ErrBadRequest},
		{name: "bad protocol token", data: strings.Replace(base, "\r\n\r\n", "\r\nSec-WebSocket-Protocol: bad;inject\r\n\r\n", 1), want: ErrBadRequest},
		{name: "oversized", data: strings.Repeat("x", DefaultMaxBytes+1), want: ErrBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := ParseServerRequest([]byte(test.data), ServerOptions{})
			if !errors.Is(err, test.want) {
				t.Fatalf("ParseServerRequest() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestBuildAndValidateClientHandshake(t *testing.T) {
	target, err := url.Parse("wss://example.test/chat?q=1")
	if err != nil {
		t.Fatal(err)
	}
	request, err := BuildClientRequest(target, testKey, []string{"chat"}, "permessage-deflate")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(request, []byte("GET /chat?q=1 HTTP/1.1\r\n")) ||
		!bytes.Contains(request, []byte("Sec-WebSocket-Protocol: chat\r\n")) {
		t.Fatalf("request = %s", request)
	}
	response := []byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" +
		"Sec-WebSocket-Protocol: chat\r\n\r\n" + "\x81\x00")
	if consumed, protocol, extensions, err := ValidateClientResponse(response, testKey, []string{"chat"}); err != nil || consumed != len(response)-2 || protocol != "chat" || len(extensions) != 0 {
		t.Fatalf("ValidateClientResponse() = %d, %q, %v, %v", consumed, protocol, extensions, err)
	}
}

func TestValidateClientResponseRejectsWrongAccept(t *testing.T) {
	response := []byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: bad\r\n\r\n")
	if _, _, _, err := ValidateClientResponse(response, testKey, nil); !errors.Is(err, ErrNotUpgrade) {
		t.Fatalf("ValidateClientResponse() error = %v, want %v", err, ErrNotUpgrade)
	}
}

func TestValidateClientResponseRejectsDuplicateSecurityHeaders(t *testing.T) {
	base := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n"
	for _, response := range []string{
		base + "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n",
		base + "Sec-WebSocket-Protocol: one\r\nSec-WebSocket-Protocol: one\r\n\r\n",
	} {
		if _, _, _, err := ValidateClientResponse([]byte(response), testKey, []string{"one"}); !errors.Is(err, ErrNotUpgrade) {
			t.Fatalf("duplicate response headers error = %v, want %v", err, ErrNotUpgrade)
		}
	}
}

func TestValidateClientResponseHonorsConfiguredLimit(t *testing.T) {
	response := []byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
	if _, _, _, err := ValidateClientResponse(response, testKey, nil, len(response)-1); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("small response limit error = %v, want %v", err, ErrBadRequest)
	}
	if _, _, _, err := ValidateClientResponse(response, testKey, nil, len(response)); err != nil {
		t.Fatalf("configured response limit rejected valid response: %v", err)
	}
}

func TestValidateClientResponseReturnsExtensions(t *testing.T) {
	response := []byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" +
		"Sec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
	if _, _, _, err := ValidateClientResponse(response, testKey, nil, len(response)-1); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("small response limit error = %v, want %v", err, ErrBadRequest)
	}
	_, _, extensions, err := ValidateClientResponse(response, testKey, nil, len(response))
	if err != nil || len(extensions) != 1 || extensions[0] != "permessage-deflate" {
		t.Fatalf("ValidateClientResponse() extensions = %v, %v", extensions, err)
	}
}

func TestHandshakeRejectsOriginAndInvalidClientInputs(t *testing.T) {
	request := []byte("GET / HTTP/1.1\r\n" +
		"Host: example.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n")
	if _, _, err := ParseServerRequest(request, ServerOptions{CheckOrigin: func(*http.Request) bool { return false }}); !errors.Is(err, ErrNotUpgrade) {
		t.Fatalf("origin rejection error = %v", err)
	}
	if _, _, err := ParseServerRequest([]byte("GET / HTTP/1.1\r\n"), ServerOptions{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("incomplete request error = %v", err)
	}
	if _, _, err := ParseServerRequest([]byte("invalid\r\n\r\n"), ServerOptions{}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("malformed request error = %v", err)
	}

	if _, err := BuildClientRequest(nil, testKey, nil, ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("nil target error = %v", err)
	}
	target, _ := url.Parse("http://example.test/socket")
	if _, err := BuildClientRequest(target, testKey, nil, ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("invalid scheme error = %v", err)
	}
	target, _ = url.Parse("ws://example.test/socket")
	if _, err := BuildClientRequest(target, "bad", nil, ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("invalid key error = %v", err)
	}
	if _, err := BuildClientRequest(target, testKey, []string{"bad token"}, ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("invalid protocol error = %v", err)
	}
}

func TestClientResponseParsingErrorPaths(t *testing.T) {
	if _, _, _, err := ValidateClientResponse([]byte("HTTP/1.1 101"), testKey, nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("incomplete response error = %v", err)
	}
	if _, _, _, err := ValidateClientResponse([]byte("invalid\r\n\r\n"), testKey, nil); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("malformed response error = %v", err)
	}
	response := []byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" +
		"Sec-WebSocket-Protocol: unoffered\r\n\r\n")
	if _, _, _, err := ValidateClientResponse(response, testKey, []string{"offered"}); !errors.Is(err, ErrNotUpgrade) {
		t.Fatalf("unoffered protocol error = %v", err)
	}
	if selected := SelectSubprotocol([]string{"one"}, []string{"two"}); selected != "" {
		t.Fatalf("unexpected selected protocol = %q", selected)
	}
}

func FuzzParseServerRequestNeverPanics(f *testing.F) {
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"))
	f.Add([]byte("GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("handshake parser panicked: %v", recovered)
			}
		}()
		_, _, _ = ParseServerRequest(data, ServerOptions{})
	})
}

func BenchmarkParseServerRequest(b *testing.B) {
	request := []byte("GET /ws HTTP/1.1\r\n" +
		"Host: 127.0.0.1:26001\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + testKey + "\r\n\r\n")
	b.ReportAllocs()
	b.SetBytes(int64(len(request)))
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := ParseServerRequest(request, ServerOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidateClientResponse(b *testing.B) {
	response := []byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" +
		"Sec-WebSocket-Protocol: chat\r\n" +
		"Sec-WebSocket-Extensions: permessage-deflate; client_no_context_takeover\r\n\r\n")
	b.ReportAllocs()
	b.SetBytes(int64(len(response)))
	b.ResetTimer()
	for b.Loop() {
		if _, _, _, err := ValidateClientResponse(response, testKey, []string{"chat"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildServerResponse(b *testing.B) {
	req := Request{Key: testKey}
	size := ServerResponseSize("chat", "permessage-deflate")
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for b.Loop() {
		response := BuildServerResponse(req, "chat", "permessage-deflate")
		if len(response) != size {
			b.Fatal(len(response))
		}
	}
}

func BenchmarkAppendServerResponse(b *testing.B) {
	req := Request{Key: testKey}
	size := ServerResponseSize("chat", "permessage-deflate")
	dst := make([]byte, 0, size)
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for b.Loop() {
		dst = AppendServerResponse(dst[:0], req, "chat", "permessage-deflate")
		if len(dst) != size {
			b.Fatal(len(dst))
		}
	}
}
