package admin

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/codex2api/auth"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCodexTestRefreshLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		preExpired, allow         bool
		code                      string
		wantRefresh, wantRequests int
	}{
		{"expired before send", true, true, "", 1, 1},
		{"server reports expired", false, true, "token_expired", 1, 2},
		{"invalid token is not retried", false, true, "invalid_token", 0, 1},
		{"recycle bin does not refresh", true, false, "token_expired", 0, 1},
		{"preflight refresh then 401 has no loop", true, true, "token_expired", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &auth.Account{AccessToken: "old-token", RefreshToken: "refresh-secret", ExpiresAt: time.Now().Add(time.Hour)}
			if tc.preExpired {
				a.ExpiresAt = time.Now().Add(-time.Hour)
			}
			refreshes, requests := 0, 0
			refresh := func() error {
				refreshes++
				a.AccessToken = "new-token"
				a.ExpiresAt = time.Now().Add(time.Hour)
				return nil
			}
			body := fmt.Sprintf(`{"error":{"code":%q}}`, tc.code)
			execute := func() (*http.Response, error) {
				requests++
				if requests == 1 && tc.code != "" {
					return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			}
			resp, err := executeCodexTestWithRefresh(context.Background(), a, tc.allow, execute, refresh)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if refreshes != tc.wantRefresh || requests != tc.wantRequests {
				t.Fatalf("refreshes=%d requests=%d", refreshes, requests)
			}
			if resp.StatusCode == 401 {
				got, _ := io.ReadAll(resp.Body)
				if string(got) != body {
					t.Fatal("401 body lost")
				}
			}
		})
	}
}
func TestCodexTestExpiryUsesJWT(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(-time.Hour).Unix())))
	a := &auth.Account{AccessToken: "e30." + payload + ".sig", ExpiresAt: time.Now().Add(time.Hour)}
	if !codexTestTokenNeedsRefresh(a, time.Now()) {
		t.Fatal("stale metadata hid expired JWT")
	}
}
func TestCodexTestConcurrentRefreshReusesToken(t *testing.T) {
	a := &auth.Account{AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour)}
	requests := 0
	resp, err := executeCodexTestWithRefresh(context.Background(), a, true, func() (*http.Response, error) {
		requests++
		if requests == 1 {
			a.AccessToken = "already-refreshed"
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"detail":{"code":"token_expired"}}`))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}, func() error { t.Fatal("redundant refresh"); return nil })
	if err != nil || requests != 2 {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
	resp.Body.Close()
}
