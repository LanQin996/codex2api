package security

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestContentSecurityPolicyCoversInlineScripts guards against the CSP hash
// drifting from frontend/index.html (the theme bootstrap script is inline).
func TestContentSecurityPolicyCoversInlineScripts(t *testing.T) {
	html, err := os.ReadFile("../frontend/index.html")
	if err != nil {
		t.Fatalf("read frontend/index.html: %v", err)
	}
	// The Linux build and Docker context use LF; normalize CRLF checkouts so the
	// test computes the same hash as the served asset.
	html = bytes.ReplaceAll(html, []byte("\r\n"), []byte("\n"))
	re := regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)
	matches := re.FindAllSubmatch(html, -1)
	if len(matches) == 0 {
		t.Fatal("no inline scripts found in frontend/index.html")
	}
	inline := 0
	for i, match := range matches {
		if strings.Contains(strings.ToLower(string(match[1])), "src=") {
			continue
		}
		script := strings.TrimSpace(string(match[2]))
		if script == "" {
			continue
		}
		inline++
		sum := sha256.Sum256(match[2])
		hash := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
		if !strings.Contains(contentSecurityPolicy, hash) {
			t.Errorf("inline script %d hash %s missing from CSP", i, hash)
		}
	}
	if inline == 0 {
		t.Fatal("no inline script content found in frontend/index.html")
	}
}
