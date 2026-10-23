package admin

import "testing"

func TestSafeInlineImageMimeType(t *testing.T) {
	tests := []struct {
		input      string
		wantType   string
		wantInline bool
	}{
		{"image/png", "image/png", true},
		{"IMAGE/JPEG; charset=binary", "image/jpeg", true},
		{"image/webp", "image/webp", true},
		{"image/gif", "image/gif", true},
		{"text/html", "application/octet-stream", false},
		{"image/svg+xml", "application/octet-stream", false},
		{"", "application/octet-stream", false},
	}
	for _, test := range tests {
		gotType, gotInline := safeInlineImageMimeType(test.input)
		if gotType != test.wantType || gotInline != test.wantInline {
			t.Errorf("safeInlineImageMimeType(%q) = (%q, %v), want (%q, %v)",
				test.input, gotType, gotInline, test.wantType, test.wantInline)
		}
	}
}
