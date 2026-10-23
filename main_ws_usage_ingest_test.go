package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/admin"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/wsrelay"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// Exercise the production observer wiring, not a test replacement observer:
// Manager/read pump -> idle quota ingest -> scheduler -> authenticated live API.
// No background probes run, and no second business request rescues the last frame.
func TestMainPersistentWebSocketIngestsLastPostTerminalUsageFrame(t *testing.T) {
	for _, test := range []struct {
		name      string
		eventType string
		contended bool
		disabled  bool
	}{
		{name: "rate limits", eventType: "codex.rate_limits"},
		{name: "response metadata", eventType: "codex.response.metadata"},
		{name: "last frame waits for observation", eventType: "codex.rate_limits", contended: true},
		{name: "kill switch", eventType: "codex.rate_limits", disabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "usage.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			tc := cache.NewMemory(1)
			t.Cleanup(func() { _ = tc.Close() })
			store := auth.NewStore(db, tc, &database.SystemSettings{MaxConcurrency: 1})
			t.Cleanup(store.Stop)
			id, err := db.InsertATAccount(context.Background(), "synthetic-ws-usage", "synthetic-access-token", "")
			if err != nil {
				t.Fatal(err)
			}
			account := &auth.Account{DBID: id, AccessToken: "synthetic-access-token", PlanType: "plus", Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}
			store.AddAccount(account)
			store.SetGlobalAutoPauseThresholds(.95, 0)
			if !account.IsAvailable() || store.AvailableCount() != 1 {
				t.Fatal("synthetic account must be dispatchable before its last frame")
			}
			if selected := store.Next(); selected != account {
				t.Fatal("scheduler did not dispatch the initial synthetic account")
			} else {
				store.Release(selected)
			}
			configureCodexIdleUsageObserver(store)
			t.Cleanup(func() { wsrelay.SetIdleMetadataObserver(nil) })
			proxy.SetCodexUsageFrameIngestEnabled(!test.disabled)
			t.Cleanup(func() { proxy.SetCodexUsageFrameIngestEnabled(true) })

			const adminKey = "synthetic-ws-usage-admin"
			adminHandler := admin.NewHandler(store, db, tc, proxy.NewRateLimiter(0), adminKey)
			router := gin.New()
			adminHandler.RegisterRoutes(router)
			liveServer := httptest.NewServer(router)
			t.Cleanup(liveServer.Close)
			client := &http.Client{Timeout: time.Second}
			readLive := func() auth.AccountLiveUsageSnapshot {
				t.Helper()
				request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/admin/accounts/live?ids=%d", liveServer.URL, id), nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("X-Admin-Key", adminKey)
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				var body struct {
					Accounts map[int64]struct {
						Status         string   `json:"status"`
						Percent        *float64 `json:"usage_percent_5h"`
						Reset          string   `json:"reset_5h_at"`
						Updated        string   `json:"codex_5h_usage_updated_at"`
						FrameAt        string   `json:"codex_usage_frame_at"`
						FrameKind      string   `json:"codex_usage_frame_kind"`
						Plan           string   `json:"plan_type"`
						CreditsValid   bool     `json:"credits_valid"`
						CreditsBalance *string  `json:"credits_balance"`
						HasCredits     bool     `json:"credits_has_credits"`
					} `json:"accounts"`
				}
				if response.StatusCode != http.StatusOK {
					t.Fatalf("authenticated live status = %d", response.StatusCode)
				}
				if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				item, ok := body.Accounts[id]
				if !ok {
					t.Fatal("live endpoint omitted the synthetic account")
				}
				result := auth.AccountLiveUsageSnapshot{Status: item.Status, UsageFrameLastKind: item.FrameKind, PlanType: item.Plan, CreditsValid: item.CreditsValid, Credits: auth.CreditBalanceSnapshot{Balance: item.CreditsBalance, HasCredits: item.HasCredits}}
				if item.Percent != nil {
					result.UsagePercent5h, result.UsagePercent5hValid = *item.Percent, true
				}
				for _, field := range []struct {
					text   string
					target *time.Time
				}{{item.Reset, &result.Reset5hAt}, {item.Updated, &result.UsageUpdatedAt5h}, {item.FrameAt, &result.UsageFrameLastAt}} {
					if field.text != "" {
						*field.target, err = time.Parse(time.RFC3339, field.text)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				return result
			}
			if before := readLive(); before.Status != "active" || before.UsagePercent5hValid || !before.UsageFrameLastAt.IsZero() {
				t.Fatalf("initial live projection = %+v", before)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			t.Cleanup(cancel)
			handshake := make(chan struct{})
			sendLastFrame := make(chan struct{})
			processedLastFrame := make(chan struct{}, 1)
			serverErrors := make(chan error, 1)
			reportServerError := func(err error) {
				select {
				case serverErrors <- err:
				default:
				}
			}
			var handshakeOnce sync.Once
			var serverHandlers sync.WaitGroup
			var requests atomic.Int32
			reset := time.Now().Add(2 * time.Hour).Truncate(time.Second)
			payload := fmt.Sprintf(`{"type":"codex.rate_limits","plan_type":"pro","credits":{"has_credits":true,"unlimited":false,"balance":"3.5"},"rate_limits":{"primary":{"used_percent":96,"window_minutes":300,"reset_at":%d}}}`, reset.Unix())
			if test.eventType == "codex.response.metadata" {
				payload = fmt.Sprintf(`{"type":"codex.response.metadata","headers":{"x-codex-plan-type":"pro","x-codex-credits-has-credits":"true","x-codex-credits-unlimited":"false","x-codex-credits-balance":"3.5","x-codex-primary-used-percent":"96","x-codex-primary-window-minutes":"300","x-codex-primary-reset-at":"%d"}}`, reset.Unix())
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				serverHandlers.Add(1)
				defer serverHandlers.Done()
				conn, err := (&websocket.Upgrader{}).Upgrade(w, request, nil)
				if err != nil {
					reportServerError(err)
					return
				}
				defer conn.Close()
				handshakeOnce.Do(func() { close(handshake) })
				if _, _, err := conn.ReadMessage(); err != nil {
					reportServerError(err)
					return
				}
				requests.Add(1)
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_synthetic","status":"completed"}}`)); err != nil {
					reportServerError(err)
					return
				}
				select {
				case <-sendLastFrame:
				case <-ctx.Done():
					return
				}
				conn.SetPongHandler(func(data string) error {
					if data == "last-frame-consumed" {
						select {
						case processedLastFrame <- struct{}{}:
						default:
						}
					}
					return nil
				})
				if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
					reportServerError(err)
					return
				}
				// A control-only barrier proves the sole reader passed the final data
				// frame, even while another observer owns the usage lock. No later
				// business frame or request is sent to trigger ingestion/recovery.
				if err := conn.WriteControl(websocket.PingMessage, []byte("last-frame-consumed"), time.Now().Add(time.Second)); err != nil {
					reportServerError(err)
					return
				}
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return // Manager shutdown closes the held-open connection.
					}
					requests.Add(1)
				}
			}))
			manager := wsrelay.NewManager()
			t.Cleanup(func() {
				manager.Stop()
				cancel()
				upstream.Close()
				serverHandlers.Wait()
			})
			wc, pending, err := manager.AcquireConnection(ctx, account, "ws"+strings.TrimPrefix(upstream.URL, "http"), "synthetic-session", http.Header{}, "")
			if err != nil {
				t.Fatal(err)
			}
			waitMainUsageSignal(t, ctx, handshake, serverErrors)
			if err := wc.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","response":{"model":"synthetic-model"}}`)); err != nil {
				t.Fatal(err)
			}
			terminal := make(chan error, 1)
			terminalDone := make(chan struct{})
			go func() {
				defer close(terminalDone)
				_, data, err := wc.ReadMessage()
				if err == nil && !strings.Contains(string(data), `"type":"response.completed"`) {
					err = fmt.Errorf("unexpected terminal: %s", data)
				}
				terminal <- err
			}()
			t.Cleanup(func() { manager.Stop(); <-terminalDone })
			select {
			case err := <-terminal:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("terminal response was not delivered")
			}
			// Match the executor's terminal cleanup before the server sends its
			// connection-level frame; the retained socket has no pending request.
			session, ok := manager.GetSession(id, wc.URL, "synthetic-session", "")
			if !ok {
				t.Fatal("manager lost the persistent session")
			}
			session.RemovePendingRequest(pending.RequestID)
			manager.ReleaseConnection(wc)

			if test.contended {
				locked, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var once sync.Once
				t.Cleanup(func() { once.Do(func() { close(release) }); <-finished })
				go func() {
					defer close(finished)
					account.ApplyUsageObservation(time.Now(), func() { close(locked); <-release })
				}()
				waitMainUsageSignal(t, ctx, locked, serverErrors)
				close(sendLastFrame)
				waitMainUsageSignal(t, ctx, processedLastFrame, serverErrors)
				once.Do(func() { close(release) })
				waitMainUsageSignal(t, ctx, finished, serverErrors)
			} else {
				close(sendLastFrame)
				waitMainUsageSignal(t, ctx, processedLastFrame, serverErrors)
			}

			if test.disabled {
				if live := readLive(); live.Status != "active" || live.UsagePercent5hValid || !live.UsageFrameLastAt.IsZero() || !account.IsAvailable() {
					t.Fatalf("disabled ingestion changed state: %+v", live)
				}
			} else {
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					live := readLive()
					if live.Status == "quota_paused" && live.UsagePercent5hValid && live.UsagePercent5h == 96 && live.Reset5hAt.Equal(reset) && !live.UsageUpdatedAt5h.IsZero() && !live.UsageFrameLastAt.IsZero() && live.UsageFrameLastKind == test.eventType && live.PlanType == "pro" && live.CreditsValid && live.Credits.HasCredits && live.Credits.Balance != nil && *live.Credits.Balance == "3.5" && !account.IsAvailable() && store.AvailableCount() == 0 {
						break
					}
					select {
					case <-ticker.C:
					case err := <-serverErrors:
						t.Fatal(err)
					case <-ctx.Done():
						t.Fatalf("final idle frame never reached scheduler/live projection: %+v; available=%v", live, account.IsAvailable())
					}
				}
			}
			if !test.disabled {
				if selected := store.Next(); selected != nil {
					store.Release(selected)
					t.Fatal("scheduler dispatched an account paused by its final idle frame")
				}
			}
			if requests.Load() != 1 || session.PendingCount() != 0 || !wc.IsConnected() || manager.ConnectionCount() != 1 {
				t.Fatalf("last frame required another request or destroyed its persistent connection: requests=%d pending=%d connected=%v pooled=%d", requests.Load(), session.PendingCount(), wc.IsConnected(), manager.ConnectionCount())
			}
		})
	}
}

func waitMainUsageSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, errors <-chan error) {
	t.Helper()
	select {
	case <-signal:
	case err := <-errors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("persistent WS fixture synchronization timed out")
	}
}
