package security

import (
	"strings"
	"testing"
)

func TestMaskURLCredentials(t *testing.T) {
	tests := []string{
		"http://user:pass@proxy.example:8080",
		"http://user@proxy.example:8080",
		"http://user:p@ss@proxy.example:8080",
		"http://user:p@ss@%zz",
		"prefix http://user:pass@proxy.example:8080 suffix",
		"http://user:pa/ss@proxy.example:8080",
		"prefix http://user:pa/ss@proxy.example:8080 suffix",
	}
	for _, input := range tests {
		got := MaskURLCredentials(input)
		for _, secret := range []string{"user", "pass", "p@ss", "pa/ss", "ss@"} {
			if strings.Contains(got, secret) {
				t.Errorf("MaskURLCredentials(%q) = %q, still contains %q", input, got, secret)
			}
		}
	}
}
