package handshake

import "bytes"

// Fast-path header names and request-line shape. Package-level slices keep the
// comparisons with bytes.EqualFold allocation-free.
var (
	fastRequestMethod = []byte("GET ")
	fastRequestProto  = []byte(" HTTP/1.1")

	fastHeaderHost           = []byte("host")
	fastHeaderUpgrade        = []byte("upgrade")
	fastHeaderConnection     = []byte("connection")
	fastHeaderVersion        = []byte("sec-websocket-version")
	fastHeaderKey            = []byte("sec-websocket-key")
	fastHeaderProtocol       = []byte("sec-websocket-protocol")
	fastHeaderExtensions     = []byte("sec-websocket-extensions")
	fastHeaderContentLength  = []byte("content-length")
	fastHeaderTransferEncode = []byte("transfer-encoding")
	fastHeaderExpect         = []byte("expect")
)

// parseServerRequestFast validates one complete upgrade header block (ending
// in \r\n\r\n) without net/http and extracts the fields the upgrade response
// needs. ok=false hands the block to the net/http parser, which keeps deciding
// exactly as it always has — error values included. The fast path never
// rejects: it only accepts a request whose regular grammar it can fully vouch
// for, so anything accepted here would also have been accepted by the net/http
// path with equal fields, and everything unusual keeps the old behavior.
//
// The win is the shape of a typical upgrade request: a dozen headers, of which
// the response needs three. net/http allocates a request, a header map, a URL,
// and a bufio reader to expose all of them; here the block is scanned in place
// and only the key, subprotocols, and extension offers become strings.
func parseServerRequestFast(block []byte, options ServerOptions) (Request, bool) {
	// CheckOrigin receives a net/http request, so those handshakes go to the
	// full parser, which builds one anyway.
	if options.CheckOrigin != nil {
		return Request{}, false
	}
	line, rest, ok := nextHeaderLine(block)
	if !ok || !bytes.HasPrefix(line, fastRequestMethod) || !bytes.HasSuffix(line, fastRequestProto) ||
		len(line) <= len(fastRequestMethod)+len(fastRequestProto) {
		return Request{}, false
	}
	if !validFastTarget(line[len(fastRequestMethod) : len(line)-len(fastRequestProto)]) {
		return Request{}, false
	}

	var (
		hostSeen     bool
		upgradeOK    bool
		connectionOK bool
		versionSeen  int
		versionOK    bool
		keySeen      int
		key          string
		lengthSeen   bool
		protocols    []string
		extensions   []string
	)
	for len(rest) > 0 {
		line, rest, ok = nextHeaderLine(rest)
		if !ok {
			return Request{}, false
		}
		if len(line) == 0 {
			// The empty line terminates the block, which ends here.
			if len(rest) != 0 {
				return Request{}, false
			}
			break
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return Request{}, false
		}
		name := line[:colon]
		for i := 0; i < len(name); i++ {
			if !fastTokenByte(name[i]) {
				return Request{}, false
			}
		}
		value := fastTrimOWS(line[colon+1:])
		for i := 0; i < len(value); i++ {
			if c := value[i]; c != '\t' && (c < 0x20 || c >= 0x7f) {
				return Request{}, false
			}
		}
		switch {
		case bytes.EqualFold(name, fastHeaderHost):
			if hostSeen || !validFastHost(value) {
				return Request{}, false
			}
			hostSeen = true
		case bytes.EqualFold(name, fastHeaderUpgrade):
			upgradeOK = upgradeOK || fastValueHasToken(value, "websocket")
		case bytes.EqualFold(name, fastHeaderConnection):
			connectionOK = connectionOK || fastValueHasToken(value, "upgrade")
		case bytes.EqualFold(name, fastHeaderVersion):
			versionSeen++
			versionOK = bytes.Equal(value, []byte("13"))
		case bytes.EqualFold(name, fastHeaderKey):
			keySeen++
			key = string(value)
		case bytes.EqualFold(name, fastHeaderProtocol):
			protocols = append(protocols, string(value))
		case bytes.EqualFold(name, fastHeaderExtensions):
			extensions = append(extensions, string(value))
		case bytes.EqualFold(name, fastHeaderContentLength):
			// Zero is what net/http exposes as ContentLength 0, which the
			// upgrade check accepts; anything else is left to net/http.
			if lengthSeen || !bytes.Equal(value, []byte("0")) {
				return Request{}, false
			}
			lengthSeen = true
		case bytes.EqualFold(name, fastHeaderTransferEncode):
			// Any encoding, chunked included, fails the upgrade check in the
			// net/http path; let it decide.
			return Request{}, false
		case bytes.EqualFold(name, fastHeaderExpect):
			// A non-empty Expect is rejected by the net/http path; an empty
			// one passes and may stay on the fast path.
			if len(value) != 0 {
				return Request{}, false
			}
		}
	}
	if !hostSeen || !upgradeOK || !connectionOK ||
		versionSeen != 1 || !versionOK ||
		keySeen != 1 || !validKey(key) {
		return Request{}, false
	}
	tokens, err := parseTokens(protocols)
	if err != nil {
		return Request{}, false
	}
	return Request{Key: key, Subprotocols: tokens, Extensions: extensions, raw: block}, true
}

// nextHeaderLine splits one \r\n-terminated line off data.
func nextHeaderLine(data []byte) (line, rest []byte, ok bool) {
	end := bytes.IndexByte(data, '\n')
	if end <= 0 || data[end-1] != '\r' {
		return nil, nil, false
	}
	return data[:end-1], data[end+1:], true
}

// fastTrimOWS trims the optional whitespace net/http's header reader trims.
func fastTrimOWS(value []byte) []byte {
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
		value = value[1:]
	}
	for len(value) > 0 && (value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		value = value[:len(value)-1]
	}
	return value
}

// fastValueHasToken mirrors headerHasToken over a raw value: comma-separated
// tokens, each trimmed and compared case-insensitively.
func fastValueHasToken(value []byte, want string) bool {
	for len(value) > 0 {
		token := value
		if i := bytes.IndexByte(value, ','); i >= 0 {
			token, value = value[:i], value[i+1:]
		} else {
			value = nil
		}
		if fastEqualFoldASCII(fastTrimOWS(token), want) {
			return true
		}
	}
	return false
}

func fastEqualFoldASCII(value []byte, want string) bool {
	if len(value) != len(want) {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != want[i] {
			return false
		}
	}
	return true
}

// fastTokenByte accepts exactly the token bytes an HTTP header name may
// contain.
func fastTokenByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// validFastTarget accepts the origin-form request targets net/http is known to
// parse: an absolute path and a query, both over unreserved bytes and
// sub-delimiters. Everything else — percent escapes included — falls back.
func validFastTarget(target []byte) bool {
	if len(target) == 0 || target[0] != '/' {
		return false
	}
	for i := 0; i < len(target); i++ {
		c := target[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '-' || c == '.' || c == '_' || c == '~': // unreserved
		case c == '!' || c == '$' || c == '&' || c == '\'' || c == '(' || c == ')' ||
			c == '*' || c == '+' || c == ',' || c == ';' || c == '=': // sub-delims
		case c == ':' || c == '@' || c == '/' || c == '?':
		default:
			return false
		}
	}
	return true
}

// validFastHost accepts the host values net/http is known to accept: a
// registered name or an IPv4/IPv6 literal with an optional port. Anything
// outside the set is left to net/http's own validation.
func validFastHost(host []byte) bool {
	if len(host) == 0 {
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.' || c == '-' || c == ':' || c == '[' || c == ']':
		default:
			return false
		}
	}
	return true
}
