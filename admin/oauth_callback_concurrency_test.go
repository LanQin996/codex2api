package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func runOAuthCallbackTestRequest(h *Handler, path string, poll bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, path, nil)
	if poll {
		h.PollOAuthCallback(c)
	} else {
		h.OAuthCallback(c)
	}
	return rec
}

func TestOAuthCallbackConcurrentClaimAndPoll(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	h := &Handler{db: db, store: auth.NewStore(db, cache.NewMemory(1), nil)}
	h.probeUsage = func(context.Context, *auth.Account) error { return nil }
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/oauth/token") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write([]byte(`{"access_token":"test-access","refresh_token":"test-refresh","expires_in":3600}`))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(unblock)
	previous, decorator := proxy.GetResinConfig(), auth.ResinRequestDecorator
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "codex2api"})
	t.Cleanup(func() { proxy.SetResinConfig(previous); auth.ResinRequestDecorator = decorator })
	t.Cleanup(func() { db.DrainBackgroundTasks(5 * time.Second) })
	const id = "callback-concurrent"
	globalOAuthStore.set(id, &oauthSession{State: id, CodeVerifier: "test", RedirectURI: oauthDefaultRedirectURI, CreatedAt: time.Now()})
	t.Cleanup(func() { globalOAuthStore.delete(id) })
	path := "/auth/callback?code=test&state=" + id
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- runOAuthCallbackTestRequest(h, path, false) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("exchange did not start")
	}
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- runOAuthCallbackTestRequest(h, path, false) }()
	// Polling must remain responsive while the upstream exchange is blocked.
	for i := 0; i < 20; i++ {
		rec := runOAuthCallbackTestRequest(h, "/poll?session_id="+id, true)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"processing"`) {
			t.Fatalf("poll during exchange = %d %s", rec.Code, rec.Body.String())
		}
	}
	select {
	case rec := <-second:
		if rec.Code != http.StatusBadRequest {
			t.Errorf("concurrent callback = %d, want 400", rec.Code)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("concurrent callback waited for upstream instead of rejecting the claimed session")
		unblock()
		<-second
	}
	var polling sync.WaitGroup
	polling.Add(1)
	go func() {
		defer polling.Done()
		for i := 0; i < 50; i++ {
			runOAuthCallbackTestRequest(h, "/poll?session_id="+id, true)
		}
	}()
	unblock()
	if rec := <-first; rec.Code != 200 || !strings.Contains(rec.Body.String(), "授权成功") {
		t.Errorf("callback = %d %s", rec.Code, rec.Body.String())
	}
	polling.Wait()
	if got := calls.Load(); got != 1 {
		t.Errorf("token exchanges = %d, want exactly one", got)
	}
}

func TestOAuthCallbackFailureIsTerminalAndRedacted(t *testing.T) {
	for _, stage := range []string{"exchange", "missing-refresh", "admin-db", "portal-db"} {
		t.Run(stage, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "oauth.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			closed := false
			t.Cleanup(func() {
				if !closed {
					_ = db.Close()
				}
			})
			h := &Handler{db: db, store: auth.NewStore(db, cache.NewMemory(1), nil)}
			var calls atomic.Int32
			server := newOAuthExchangeTestServer(t)
			server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if stage == "exchange" {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				refresh := "test-refresh"
				if stage == "missing-refresh" {
					refresh = ""
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "test-access", "refresh_token": refresh})
			})
			if strings.HasSuffix(stage, "-db") {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				closed = true
			}
			id := "failure-" + stage
			globalOAuthStore.set(id, &oauthSession{State: id, CodeVerifier: "test", RedirectURI: oauthDefaultRedirectURI, SelfService: stage == "portal-db", CreatedAt: time.Now()})
			t.Cleanup(func() { globalOAuthStore.delete(id) })
			path := "/auth/callback?code=test&state=" + id
			rec := runOAuthCallbackTestRequest(h, path, false)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "授权失败") {
				t.Fatalf("failure page = %d %s", rec.Code, rec.Body.String())
			}
			if replay := runOAuthCallbackTestRequest(h, path, false); replay.Code != 400 {
				t.Errorf("failed session replay = %d, want 400", replay.Code)
			}
			poll := runOAuthCallbackTestRequest(h, "/poll?session_id="+id, true)
			if poll.Code != 200 || !strings.Contains(poll.Body.String(), `"completed"`) || !strings.Contains(poll.Body.String(), `"success":false`) {
				t.Errorf("failed result = %d %s", poll.Code, poll.Body.String())
			}
			for _, body := range []string{rec.Body.String(), poll.Body.String()} {
				if strings.Contains(body, "database is closed") || strings.Contains(body, "HTTP 502") {
					t.Errorf("public callback exposes internal error: %s", body)
				}
			}
			if calls.Load() != 1 {
				t.Errorf("failed session exchanged %d times", calls.Load())
			}
		})
	}
}
