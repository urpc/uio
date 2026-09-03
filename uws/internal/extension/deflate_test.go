package extension

import (
	"strings"
	"testing"
)

func TestNegotiateServerDeflate(t *testing.T) {
	params, response, err := NegotiateServer([]string{"permessage-deflate; client_max_window_bits"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !params.Enabled || !params.ServerNoContextTakeover || !params.ClientNoContextTakeover {
		t.Fatalf("params = %+v", params)
	}
	if response != "permessage-deflate; server_no_context_takeover; client_no_context_takeover; client_max_window_bits=15" {
		t.Fatalf("response = %q", response)
	}
	if !params.ClientMaxWindowBitsSet || params.ClientMaxWindowBits != 15 {
		t.Fatalf("client window bits = %+v", params)
	}
}

func TestNegotiateServerRejectsInvalidParameter(t *testing.T) {
	if _, _, err := NegotiateServer([]string{"permessage-deflate; server_max_window_bits=7"}, true); err == nil {
		t.Fatal("invalid window bits were accepted")
	}
}

func TestNegotiateClientDeflateResponse(t *testing.T) {
	params, err := NegotiateClient([]string{"permessage-deflate; server_no_context_takeover; client_no_context_takeover"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !params.Enabled || !params.ServerNoContextTakeover || !params.ClientNoContextTakeover {
		t.Fatalf("params = %+v", params)
	}
	params, err = NegotiateClient([]string{"permessage-deflate"}, true)
	if err != nil || !params.Enabled {
		t.Fatalf("context response = %+v, %v", params, err)
	}
}

func TestNegotiateServerContextPolicyMatchesResponse(t *testing.T) {
	params, response, err := NegotiateServerWithPolicy([]string{
		"permessage-deflate; server_no_context_takeover; client_max_window_bits",
	}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !params.Enabled || !params.ServerNoContextTakeover || params.ClientNoContextTakeover {
		t.Fatalf("params = %+v", params)
	}
	if response != "permessage-deflate; server_no_context_takeover; client_max_window_bits=15" {
		t.Fatalf("response = %q", response)
	}
}

func TestNegotiateServerNegotiatesWindowBits(t *testing.T) {
	params, response, err := NegotiateServer([]string{"permessage-deflate; server_max_window_bits=12"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !params.Enabled || params.ServerMaxWindowBits != 12 || response != "permessage-deflate; server_no_context_takeover; client_no_context_takeover; server_max_window_bits=12" {
		t.Fatalf("window negotiation = %+v, %q", params, response)
	}
}

func TestNegotiateServerSelectsFirstDeflateOffer(t *testing.T) {
	params, response, err := NegotiateServer([]string{
		"permessage-deflate; server_max_window_bits=12, permessage-deflate",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !params.Enabled || response != "permessage-deflate; server_no_context_takeover; client_no_context_takeover; server_max_window_bits=12" {
		t.Fatalf("first offer was not selected: %+v, %q", params, response)
	}
}

func TestNegotiateClientAcceptsWindowBits(t *testing.T) {
	params, err := NegotiateClient([]string{"permessage-deflate; client_max_window_bits=12"}, true)
	if err != nil || !params.Enabled || params.ClientMaxWindowBits != 12 {
		t.Fatalf("NegotiateClient() = %+v, %v", params, err)
	}
}

func TestNegotiateClientAcceptsServerWindowBits(t *testing.T) {
	params, err := NegotiateClient([]string{"permessage-deflate; server_max_window_bits=12"}, true)
	if err != nil || !params.Enabled || params.ServerMaxWindowBits != 12 {
		t.Fatalf("NegotiateClient() = %+v, %v", params, err)
	}
}

func TestNegotiateClientRejectsDuplicateDeflateExtension(t *testing.T) {
	if _, err := NegotiateClient([]string{"permessage-deflate, permessage-deflate"}, true); err == nil {
		t.Fatal("duplicate permessage-deflate response was accepted")
	}
}

func TestNegotiateClientRejectsTrailingUnknownExtension(t *testing.T) {
	if _, err := NegotiateClient([]string{"permessage-deflate, x-unknown"}, true); err == nil {
		t.Fatal("trailing unknown extension was accepted")
	}
}

func TestNegotiateClientRejectsValuelessResponseWindow(t *testing.T) {
	if _, err := NegotiateClient([]string{"permessage-deflate; client_max_window_bits"}, true); err == nil {
		t.Fatal("valueless client_max_window_bits response was accepted")
	}
}

func TestNegotiationDisabledAndMissingOffers(t *testing.T) {
	params, response, err := NegotiateServer([]string{"permessage-deflate"}, false)
	if err != nil || params.Enabled || response != "" {
		t.Fatalf("disabled server negotiation = %+v, %q, %v", params, response, err)
	}
	params, response, err = NegotiateServer([]string{"x-extension"}, true)
	if err != nil || params.Enabled || response != "" {
		t.Fatalf("missing server offer = %+v, %q, %v", params, response, err)
	}
	if params, err = NegotiateClient(nil, false); err != nil || params.Enabled {
		t.Fatalf("disabled client negotiation = %+v, %v", params, err)
	}
	if _, err = NegotiateClient([]string{"permessage-deflate"}, false); err == nil {
		t.Fatal("disabled client accepted extension response")
	}
	if params, err = NegotiateClient(nil, true); err != nil || params.Enabled {
		t.Fatalf("missing client response = %+v, %v", params, err)
	}
}

func TestParseParamsRejectsMalformedValues(t *testing.T) {
	tests := []string{
		"server_no_context_takeover; server_no_context_takeover",
		"server_no_context_takeover=true",
		"client_no_context_takeover=true",
		"server_max_window_bits",
		"server_max_window_bits=bad",
		"client_max_window_bits=bad",
		"unknown=value",
	}
	for _, parameters := range tests {
		t.Run(strings.ReplaceAll(parameters, "; ", "_"), func(t *testing.T) {
			if _, _, err := NegotiateServer([]string{"permessage-deflate; " + parameters}, true); err == nil {
				t.Fatalf("malformed parameters accepted: %s", parameters)
			}
		})
	}
}

func FuzzNegotiateServerNeverPanics(f *testing.F) {
	f.Add("permessage-deflate; client_max_window_bits")
	f.Add("permessage-deflate; server_max_window_bits=12, permessage-deflate")
	f.Fuzz(func(t *testing.T, value string) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("server negotiator panicked: %v", recovered)
			}
		}()
		_, _, _ = NegotiateServer([]string{value}, true)
	})
}

func FuzzNegotiateClientNeverPanics(f *testing.F) {
	f.Add("permessage-deflate; client_max_window_bits=15")
	f.Add("permessage-deflate; server_no_context_takeover")
	f.Fuzz(func(t *testing.T, value string) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("client negotiator panicked: %v", recovered)
			}
		}()
		_, _ = NegotiateClient([]string{value}, true)
	})
}
