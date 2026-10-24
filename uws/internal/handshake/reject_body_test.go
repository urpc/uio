package handshake

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// A Content-Type the caller spelled its own way is one header, not two: the
// default must not be added beside it.
func TestBuildRejectResponseSingleContentType(t *testing.T) {
	for _, name := range []string{"Content-Type", "content-type", "CONTENT-TYPE"} {
		header := http.Header{name: {"application/json"}}
		response := string(BuildRejectResponse(http.StatusForbidden, header, "no"))
		if n := strings.Count(response, "ontent-Type:"); n != 1 {
			t.Errorf("%s: %d Content-Type headers in the refusal", name, n)
		}
		if !strings.Contains(response, "application/json") {
			t.Errorf("%s: the caller's Content-Type value was dropped", name)
		}
	}
}

// The body cut ends on a whole UTF-8 character.
func TestLimitBodyEndsOnRune(t *testing.T) {
	long := strings.Repeat("界", MaxRejectBody) // three bytes each
	cut := LimitBody(long)
	if len(cut) > MaxRejectBody {
		t.Fatalf("LimitBody returned %d bytes", len(cut))
	}
	if !utf8.ValidString(cut) {
		t.Fatal("LimitBody cut a character in half")
	}
	if n := utf8.RuneCountInString(cut); n != MaxRejectBody/3 {
		t.Fatalf("LimitBody kept %d of %d characters", n, MaxRejectBody/3)
	}
}
