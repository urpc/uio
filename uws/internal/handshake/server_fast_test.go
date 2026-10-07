package handshake

import (
	"bytes"
	"net/http"
	"reflect"
	"slices"
	"testing"
)

// requireParseAgreement runs both parsers over one complete block and demands
// identical decisions, fields, and lazily built requests. Whenever the fast
// path accepts, the net/http path must accept with equal fields; whenever it
// falls back, errors must match exactly.
func requireParseAgreement(t *testing.T, data []byte) {
	t.Helper()
	end := bytes.Index(data, []byte("\r\n\r\n"))
	if end < 0 {
		t.Fatal("test block is incomplete")
	}
	end += 4
	block := data[:end]
	fast, fastConsumed, fastErr := ParseServerRequest(data, ServerOptions{})
	slow, slowConsumed, slowErr := parseServerRequestNetHTTP(block, ServerOptions{})
	switch {
	case fastErr == nil && slowErr == nil:
	case fastErr == nil || slowErr == nil:
		t.Fatalf("decision diverged: fast error %v, net/http error %v", fastErr, slowErr)
	default:
		if fastErr.Error() != slowErr.Error() {
			t.Fatalf("error diverged: fast %v, net/http %v", fastErr, slowErr)
		}
		return
	}
	if fastConsumed != end || slowConsumed != end {
		t.Fatalf("consumed = %d/%d, want %d", fastConsumed, slowConsumed, end)
	}
	if fast.Key != slow.Key ||
		!slices.Equal(fast.Subprotocols, slow.Subprotocols) ||
		!slices.Equal(fast.Extensions, slow.Extensions) {
		t.Fatalf("fields diverged: fast %+v, net/http %+v", fast, slow)
	}
	fastHTTP, err := fast.BuildHTTP()
	if err != nil {
		t.Fatalf("fast BuildHTTP: %v", err)
	}
	assertRequestsEqual(t, fastHTTP, slow.HTTP)
}

func assertRequestsEqual(t *testing.T, got, want *http.Request) {
	t.Helper()
	if got.Method != want.Method || got.RequestURI != want.RequestURI ||
		got.Host != want.Host || got.Proto != want.Proto ||
		got.ContentLength != want.ContentLength ||
		!slices.Equal(got.TransferEncoding, want.TransferEncoding) {
		t.Fatalf("request diverged: got %+v, want %+v", got, want)
	}
	if (got.URL == nil) != (want.URL == nil) ||
		(got.URL != nil && got.URL.String() != want.URL.String()) {
		t.Fatalf("URL diverged: %v vs %v", got.URL, want.URL)
	}
	if !reflect.DeepEqual(got.Header, want.Header) {
		t.Fatalf("header diverged: got %v, want %v", got.Header, want.Header)
	}
}

func TestParseServerRequestFastPathAgreement(t *testing.T) {
	const (
		unknown = 0
		reject  = -1
		accept  = 1
	)
	host := "Host: 127.0.0.1:8080\r\n"
	upgrade := "Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n"
	cases := []struct {
		name     string
		request  string
		fastPath bool // the fast parser itself must accept the block
		verdict  int  // accept/reject, or unknown to only require agreement
	}{
		{name: "minimal", request: "GET / HTTP/1.1\r\n" + host + upgrade + "\r\n", fastPath: true, verdict: accept},
		{
			name: "typical",
			request: "GET /ws HTTP/1.1\r\n" + host +
				"User-Agent: Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36\r\n" +
				"Accept-Encoding: gzip, deflate, br, zstd\r\n" +
				"Accept-Language: en-US,en;q=0.9\r\n" +
				"Cache-Control: no-cache\r\n" +
				"Pragma: no-cache\r\n" +
				"Origin: http://127.0.0.1:8080\r\n" +
				"X-Custom-Trace: business-42\r\n" +
				upgrade +
				"Sec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n\r\n",
			fastPath: true, verdict: accept,
		},
		{name: "no-space-after-colon", request: "GET / HTTP/1.1\r\n" + host + "Upgrade:websocket\r\nConnection:Upgrade\r\nSec-WebSocket-Version:13\r\nSec-WebSocket-Key:" + testKey + "\r\n\r\n", fastPath: true, verdict: accept},
		{name: "tab-after-colon", request: "GET / HTTP/1.1\r\n" + host + "Upgrade:\twebsocket\r\nConnection:\tUpgrade\r\nSec-WebSocket-Version:\t13\r\nSec-WebSocket-Key:\t" + testKey + "\r\n\r\n", fastPath: true, verdict: accept},
		{name: "mixed-case-values", request: "GET / HTTP/1.1\r\n" + host + "uPgRaDe: WebSocket\r\nCONNECTION: upgradE\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n", fastPath: true, verdict: accept},
		{name: "connection-token-list", request: "GET / HTTP/1.1\r\n" + host + "Connection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n", fastPath: true, verdict: accept},
		{name: "upgrade-token-list", request: "GET / HTTP/1.1\r\n" + host + "Upgrade: h2c, websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n", fastPath: true, verdict: accept},
		{name: "duplicate-upgrade-lines", request: "GET / HTTP/1.1\r\n" + host + "Upgrade: h2c\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n", fastPath: true, verdict: accept},
		{name: "version-padded", request: "GET / HTTP/1.1\r\n" + host + "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version:  13 \r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n", fastPath: true, verdict: accept},
		{name: "version-duplicate", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Sec-WebSocket-Version: 13\r\n\r\n", verdict: reject},
		{name: "version-wrong", request: "GET / HTTP/1.1\r\n" + host + "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 8\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n", verdict: reject},
		{name: "key-padded", request: "GET / HTTP/1.1\r\n" + host + "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key:  " + testKey + "  \r\n\r\n", fastPath: true, verdict: accept},
		{name: "key-invalid", request: "GET / HTTP/1.1\r\n" + host + "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: abc\r\n\r\n", verdict: reject},
		{name: "key-duplicate", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Sec-WebSocket-Key: " + testKey + "\r\n\r\n", verdict: reject},
		{name: "query-target", request: "GET /ws?a=1&b=two%20three HTTP/1.1\r\n" + host + upgrade + "\r\n", verdict: unknown},
		{name: "query-target-plain", request: "GET /ws?a=1&b=two-three HTTP/1.1\r\n" + host + upgrade + "\r\n", fastPath: true, verdict: accept},
		{name: "ipv6-host", request: "GET / HTTP/1.1\r\nHost: [::1]:8080\r\n" + upgrade + "\r\n", fastPath: true, verdict: accept},
		// net/http's ReadRequest itself neither requires a Host header nor
		// rejects an empty one; those requests stay on the net/http path.
		{name: "host-empty", request: "GET / HTTP/1.1\r\nHost: \r\n" + upgrade + "\r\n", verdict: accept},
		{name: "host-missing", request: "GET / HTTP/1.1\r\n" + upgrade + "\r\n", verdict: accept},
		{name: "host-duplicate", request: "GET / HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nHost: other.test\r\n" + upgrade + "\r\n", verdict: unknown},
		{name: "host-underscore", request: "GET / HTTP/1.1\r\nHost: my_host:8080\r\n" + upgrade + "\r\n", verdict: unknown},
		{name: "http-1-0", request: "GET / HTTP/1.0\r\n" + host + upgrade + "\r\n", verdict: reject},
		{name: "post-method", request: "POST / HTTP/1.1\r\n" + host + upgrade + "\r\n", verdict: reject},
		{name: "lowercase-get", request: "get / HTTP/1.1\r\n" + host + upgrade + "\r\n", verdict: reject},
		{name: "missing-upgrade", request: "GET / HTTP/1.1\r\n" + host + "Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n", verdict: reject},
		{name: "upgrade-wrong-token", request: "GET / HTTP/1.1\r\n" + host + "Upgrade: h2c\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n", verdict: reject},
		{name: "missing-connection", request: "GET / HTTP/1.1\r\n" + host + "Upgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n", verdict: reject},
		{name: "content-length-zero", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Content-Length: 0\r\n\r\n", fastPath: true, verdict: accept},
		{name: "content-length-padded-zero", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Content-Length: 00\r\n\r\n", verdict: accept},
		{name: "content-length-five", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Content-Length: 5\r\n\r\n", verdict: reject},
		{name: "transfer-encoding", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Transfer-Encoding: chunked\r\n\r\n", verdict: reject},
		{name: "expect-continue", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Expect: 100-continue\r\n\r\n", verdict: reject},
		{name: "expect-empty", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Expect:\r\n\r\n", fastPath: true, verdict: accept},
		{name: "absolute-form", request: "GET http://127.0.0.1:8080/ws HTTP/1.1\r\n" + host + upgrade + "\r\n", verdict: unknown},
		{name: "asterisk-form", request: "GET * HTTP/1.1\r\n" + host + upgrade + "\r\n", verdict: unknown},
		{name: "obs-fold", request: "GET / HTTP/1.1\r\n" + host + upgrade + "X-Folded: one\r\n two\r\n\r\n", verdict: unknown},
		{name: "target-percent-escape", request: "GET /%zz HTTP/1.1\r\n" + host + upgrade + "\r\n", verdict: reject},
		{name: "target-space", request: "GET /a b HTTP/1.1\r\n" + host + upgrade + "\r\n", verdict: reject},
		// Raw non-ASCII target bytes parse in net/http too; the fast path
		// leaves them to it.
		{name: "target-utf8", request: "GET /caf\xc3\xa9 HTTP/1.1\r\n" + host + upgrade + "\r\n", verdict: accept},
		{name: "header-underscore", request: "GET / HTTP/1.1\r\n" + host + "X_Custom-Trace: 1\r\n" + upgrade + "\r\n", fastPath: true, verdict: accept},
		{name: "header-name-at-sign", request: "GET / HTTP/1.1\r\n" + host + "X@Trace: 1\r\n" + upgrade + "\r\n", verdict: reject},
		{name: "header-empty-value", request: "GET / HTTP/1.1\r\n" + host + "X-Empty:\r\n" + upgrade + "\r\n", fastPath: true, verdict: accept},
		{name: "header-non-ascii-value", request: "GET / HTTP/1.1\r\n" + host + "X-Name: caf\xc3\xa9\r\n" + upgrade + "\r\n", verdict: unknown},
		{name: "header-control-value", request: "GET / HTTP/1.1\r\n" + host + "X-Name: a\x01b\r\n" + upgrade + "\r\n", verdict: unknown},
		{name: "bare-lf-header", request: "GET / HTTP/1.1\r\n" + host + "Upgrade: websocket\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n", verdict: unknown},
		{name: "subprotocols", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Sec-WebSocket-Protocol: chat, superchat\r\n\r\n", fastPath: true, verdict: accept},
		{name: "subprotocols-multiple-lines", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Sec-WebSocket-Protocol: chat\r\nSec-WebSocket-Protocol: superchat\r\n\r\n", fastPath: true, verdict: accept},
		{name: "subprotocol-invalid-token", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Sec-WebSocket-Protocol: bad token\r\n\r\n", verdict: reject},
		{name: "extensions-multiple-lines", request: "GET / HTTP/1.1\r\n" + host + upgrade + "Sec-WebSocket-Extensions: permessage-deflate\r\nSec-WebSocket-Extensions: x-custom; p=1\r\n\r\n", fastPath: true, verdict: accept},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.request)
			requireParseAgreement(t, data)
			_, _, err := ParseServerRequest(data, ServerOptions{})
			if accepted := err == nil; tc.verdict != unknown && accepted != (tc.verdict == accept) {
				t.Fatalf("accepted = %v, want %v (err %v)", accepted, tc.verdict == accept, err)
			}
			end := bytes.Index(data, []byte("\r\n\r\n")) + 4
			_, fastOK := parseServerRequestFast(data[:end], ServerOptions{})
			if fastOK != tc.fastPath {
				t.Fatalf("fast path engaged = %v, want %v", fastOK, tc.fastPath)
			}
		})
	}
}

func TestParseServerRequestFastPathCheckOriginFallsBack(t *testing.T) {
	request := []byte("GET / HTTP/1.1\r\nHost: 127.0.0.1:8080\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n")
	end := bytes.Index(request, []byte("\r\n\r\n")) + 4
	if _, fastOK := parseServerRequestFast(request[:end], ServerOptions{}); !fastOK {
		t.Fatal("fast path did not engage without CheckOrigin")
	}
	options := ServerOptions{CheckOrigin: func(*http.Request) bool { return true }}
	if _, fastOK := parseServerRequestFast(request[:end], options); fastOK {
		t.Fatal("fast path engaged with CheckOrigin set")
	}
	result, _, err := ParseServerRequest(request, options)
	if err != nil {
		t.Fatalf("ParseServerRequest with CheckOrigin: %v", err)
	}
	if result.HTTP == nil {
		t.Fatal("CheckOrigin result did not carry a net/http request")
	}
}

func FuzzParseServerRequestFastMatchesNetHTTP(f *testing.F) {
	f.Add([]byte("GET / HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"))
	f.Add([]byte("GET /ws?a=1 HTTP/1.1\r\nHost: [::1]:80\r\nUpgrade: h2c, websocket\r\nConnection: keep-alive, Upgrade\r\nContent-Length: 0\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"))
	f.Add([]byte("GET /%zz HTTP/1.1\r\nHost: a-b.example\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 8\r\nSec-WebSocket-Key: abc\r\n\r\n"))
	f.Add([]byte("GET / HTTP/1.1\r\nHost: example.test\r\nUpgrade : websocket\r\n\r\n"))
	f.Add([]byte("POST http://ex.test/ HTTP/1.1\r\nHost: ex.test\r\nExpect: 100-continue\r\nUpgrade: websocket\nConnection: Upgrade\r\n\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		end := bytes.Index(data, []byte("\r\n\r\n"))
		if end < 0 {
			_, _, _ = ParseServerRequest(data, ServerOptions{})
			return
		}
		end += 4
		block := data[:end]
		fast, fastConsumed, fastErr := ParseServerRequest(data, ServerOptions{})
		slow, _, slowErr := parseServerRequestNetHTTP(block, ServerOptions{})
		if (fastErr == nil) != (slowErr == nil) {
			t.Fatalf("decision diverged: fast error %v, net/http error %v (block %q)", fastErr, slowErr, block)
		}
		if fastErr != nil {
			return
		}
		if fastConsumed != end {
			t.Fatalf("consumed = %d, want %d", fastConsumed, end)
		}
		if fast.Key != slow.Key ||
			!slices.Equal(fast.Subprotocols, slow.Subprotocols) ||
			!slices.Equal(fast.Extensions, slow.Extensions) {
			t.Fatalf("fields diverged: fast %+v, net/http %+v (block %q)", fast, slow, block)
		}
		fastHTTP, err := fast.BuildHTTP()
		if err != nil {
			t.Fatalf("fast BuildHTTP: %v (block %q)", err, block)
		}
		assertRequestsEqual(t, fastHTTP, slow.HTTP)
	})
}
