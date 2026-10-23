package proxy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func TestAntigravityDisabledModelMarker(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"gemini-4-argon-disabled", true},
		{" Future_Model_DISABLED_low ", true},
		{"disabled-model", true},
		{"gemini-4-argon-high", false},
		{"model-nondisabled", false},
		{"model-disabledness", false},
		{"", false},
	} {
		if got := AntigravityDisabledModel(tc.model); got != tc.want {
			t.Errorf("disabled marker for %q = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestAntigravityAdminCatalogKeepsDisabledPlaceholders(t *testing.T) {
	raw := []string{
		"gemini-4-argon-disabled", "Future_Model_DISABLED_low",
		"tab_hidden-disabled", "imagen-4-disabled", "gemini-4-image-disabled",
		"gemini bad-disabled", strings.Repeat("x", 201) + "-disabled",
	}
	want := []string{"Future_Model_DISABLED_low", "gemini-4-argon-disabled"}
	if got := AntigravityAdminModelIDs(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("admin display catalog = %v, want %v", got, want)
	}
	if got := AntigravityPublishedModelIDs(raw); len(got) != 0 {
		t.Fatalf("unavailable placeholders advertised for dispatch: %v", got)
	}
	account := &auth.Account{UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", Models: raw}
	for _, model := range want {
		if _, supported := antigravityResolvePublicModelForAccount(account, model); supported {
			t.Fatalf("admin visibility enabled ordinary dispatch for %s", model)
		}
	}

	// A later provider snapshot replaces the placeholder rather than retaining
	// a disabled flag on the now-available model family.
	refreshed := []string{"gemini-4-argon-high", "Future_Model_low"}
	wantRefreshed := []string{"gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-argon-high", "Future_Model_low"}
	if got := AntigravityAdminModelIDs(refreshed); !reflect.DeepEqual(got, wantRefreshed) {
		t.Fatalf("refreshed admin catalog = %v, want %v", got, wantRefreshed)
	}
	for _, model := range AntigravityAdminModelIDs(refreshed) {
		if AntigravityDisabledModel(model) {
			t.Fatalf("stale disabled state survived refresh: %s", model)
		}
	}
	account.Models = refreshed
	if !antigravityAccountSupportsPublicModel(account, "gemini-4-argon-high") {
		t.Fatal("refreshed callable backing remained unavailable")
	}
}
