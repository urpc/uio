package handshake

import (
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestExtraHeadersPreserveHandshakeFraming(t *testing.T) {
	for _, check := range []struct {
		name     string
		fn       func(http.Header) error
		reserved string
	}{{"request", CheckRequestHeader, "sEc-WeBsOcKeT-kEy"}, {"reject", CheckRejectHeader, "cOnTeNt-LeNgTh"}} {
		t.Run(check.name, func(t *testing.T) {
			for _, header := range []http.Header{nil, {"X-Trace": {"one", "two"}}} {
				if err := check.fn(header); err != nil {
					t.Fatalf("valid headers: %v", err)
				}
			}
			for _, header := range []http.Header{
				{check.reserved: {"override"}}, {"bad name": {"value"}},
				{"X-Trace": {"ok", "bad\r\nInjected: yes"}}, {"X-Trace": {"bad\x00value"}},
			} {
				if err := check.fn(header); err == nil {
					t.Fatalf("unsafe header accepted: %#v", header)
				}
			}
		})
	}
}

func TestHandshakeRefusalAndLazyRequestContracts(t *testing.T) {
	err := &ResponseError{StatusCode: 429}
	if !errors.Is(err, ErrNotUpgrade) || !strings.Contains(err.Error(), "429") {
		t.Fatalf("refusal identity/message: %v", err)
	}
	r := Request{}
	if _, err := r.BuildHTTP(); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("missing retained header: %v", err)
	}
	r.raw = []byte("GET / HTTP/not-a-version\r\n\r\n")
	if _, err := r.BuildHTTP(); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("corrupt retained header: %v", err)
	}
	key := strings.Repeat("challenge", 16)
	sum := sha1.Sum([]byte(key + webSocketGUID))
	if got, want := string(appendAcceptKey([]byte("prefix:"), key)), "prefix:"+base64.StdEncoding.EncodeToString(sum[:]); got != want {
		t.Fatalf("long challenge: %q, want %q", got, want)
	}
}
