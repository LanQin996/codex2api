package credentialops

import (
	"strings"
	"testing"
)

func TestBlockedCodeClassifier(t *testing.T) {
	for _, s := range []string{`403 forbidden`, `429 rate limited`, `Cloudflare account disabled`, `{"error":{"code":"invalid_grant"}}`, `{"code":"organization_deactivated"}`, `{"code":"token_expired"}`} {
		if IsAccountBlockedResponse(s) {
			t.Fatalf("false ban: %s", s)
		}
	}
	input := `[error] HTTP 401: {"error":{"code":"account_deactivated","message":"synthetic-private"}}`
	if !IsAccountBlockedResponse(input) {
		t.Fatal("missed explicit code")
	}
	for split := 0; split < len(input); split++ {
		var w blockedOutput
		w.Write([]byte(input[:split]))
		w.Write([]byte(input[split:]))
		w.Write([]byte(strings.Repeat("x", 100000)))
		if !w.blocked || len(w.tail) > 256 {
			t.Fatal("stream detection or bound failed")
		}
	}
}
