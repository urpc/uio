package extension

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/urpc/uio/uws/internal/compress"
)

const perMessageDeflate = "permessage-deflate"

func NegotiateServer(values []string, enabled bool) (compress.Params, string, error) {
	return NegotiateServerWithPolicy(values, enabled, true)
}

func NegotiateServerWithPolicy(values []string, enabled, noContextTakeover bool) (compress.Params, string, error) {
	if !enabled {
		return compress.Params{}, "", nil
	}
	for _, value := range values {
		for _, raw := range strings.Split(value, ",") {
			parts := strings.Split(raw, ";")
			if !strings.EqualFold(strings.TrimSpace(parts[0]), perMessageDeflate) {
				continue
			}
			params, err := parseParams(parts[1:])
			if err != nil {
				return compress.Params{}, "", err
			}
			params.Enabled = true
			serverNoContext := noContextTakeover || params.ServerNoContextTakeover
			clientNoContext := noContextTakeover || params.ClientNoContextTakeover
			params.ServerNoContextTakeover = serverNoContext
			params.ClientNoContextTakeover = clientNoContext
			var response strings.Builder
			response.WriteString(perMessageDeflate)
			if serverNoContext {
				response.WriteString("; server_no_context_takeover")
			}
			if clientNoContext {
				response.WriteString("; client_no_context_takeover")
			}
			if params.ServerMaxWindowBitsSet {
				response.WriteString("; server_max_window_bits=")
				response.WriteString(strconv.Itoa(params.ServerMaxWindowBits))
			}
			if params.ClientMaxWindowBitsSet {
				if params.ClientMaxWindowBits == 0 {
					params.ClientMaxWindowBits = compress.DefaultWindowBits
				}
				response.WriteString("; client_max_window_bits=")
				response.WriteString(strconv.Itoa(params.ClientMaxWindowBits))
			}
			return params, response.String(), nil
		}
	}
	return compress.Params{}, "", nil
}

func NegotiateClient(values []string, requested bool) (compress.Params, error) {
	if !requested {
		if len(values) != 0 {
			return compress.Params{}, fmt.Errorf("websocket: unexpected extension response")
		}
		return compress.Params{}, nil
	}
	if len(values) == 0 {
		return compress.Params{}, nil
	}
	var negotiated compress.Params
	found := false
	for _, value := range values {
		for _, raw := range strings.Split(value, ",") {
			parts := strings.Split(raw, ";")
			if !strings.EqualFold(strings.TrimSpace(parts[0]), perMessageDeflate) {
				return compress.Params{}, fmt.Errorf("websocket: unexpected extension %q", strings.TrimSpace(parts[0]))
			}
			if found {
				return compress.Params{}, fmt.Errorf("websocket: duplicate extension %q", perMessageDeflate)
			}
			params, err := parseParams(parts[1:])
			if err != nil {
				return compress.Params{}, err
			}
			if params.ClientMaxWindowBitsSet && params.ClientMaxWindowBits == 0 {
				return compress.Params{}, fmt.Errorf("websocket: valueless client_max_window_bits response")
			}
			params.Enabled = true
			negotiated = params
			found = true
		}
	}
	return negotiated, nil
}

func parseParams(parts []string) (compress.Params, error) {
	params := compress.Params{Level: -1}
	seen := make(map[string]bool, len(parts))
	for _, raw := range parts {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		kv := strings.SplitN(item, "=", 2)
		name := strings.ToLower(strings.TrimSpace(kv[0]))
		if seen[name] {
			return compress.Params{}, fmt.Errorf("websocket: duplicate deflate parameter %q", name)
		}
		seen[name] = true
		switch name {
		case "server_no_context_takeover":
			if len(kv) != 1 {
				return compress.Params{}, fmt.Errorf("websocket: invalid %s", name)
			}
			params.ServerNoContextTakeover = true
		case "client_no_context_takeover":
			if len(kv) != 1 {
				return compress.Params{}, fmt.Errorf("websocket: invalid %s", name)
			}
			params.ClientNoContextTakeover = true
		case "server_max_window_bits":
			if len(kv) != 2 {
				return compress.Params{}, fmt.Errorf("websocket: missing %s value", name)
			}
			bits, err := strconv.Atoi(strings.TrimSpace(kv[1]))
			if err != nil || bits < 8 || bits > 15 {
				return compress.Params{}, fmt.Errorf("websocket: invalid %s", name)
			}
			params.ServerMaxWindowBitsSet = true
			params.ServerMaxWindowBits = bits
		case "client_max_window_bits":
			params.ClientMaxWindowBitsSet = true
			if len(kv) == 2 {
				bits, err := strconv.Atoi(strings.TrimSpace(kv[1]))
				if err != nil || bits < 8 || bits > 15 {
					return compress.Params{}, fmt.Errorf("websocket: invalid %s", name)
				}
				params.ClientMaxWindowBits = bits
			}
		default:
			return compress.Params{}, fmt.Errorf("websocket: unknown deflate parameter %q", name)
		}
	}
	return params, nil
}
