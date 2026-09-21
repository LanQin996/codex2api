package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func testTurnStateValue(blocks int) string {
	if blocks <= 0 {
		blocks = 10
	}
	raw := make([]byte, 1+8+16+blocks*16+32)
	raw[0] = 0x80
	for i := 1; i <= 8; i++ {
		raw[i] = 0
	}
	for i := 9; i < len(raw); i++ {
		raw[i] = byte(i % 251)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func ticketHarvesterFixture(t *testing.T) (*CodexTurnStateHarvester, *auth.Account) {
	t.Helper()
	oldConfig := CurrentCodexTurnStateTicketConfig()
	oldHarvester := activeCodexTurnStateHarvester.Load()
	t.Cleanup(func() { SetCodexTurnStateTicketConfig(oldConfig); activeCodexTurnStateHarvester.Store(oldHarvester) })
	SetCodexTurnStateTicketConfig(&CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-*"}, TargetLength: len(testTurnStateValue(10)), TTLSeconds: 3600, RefreshBeforeSeconds: 600})
	account := &auth.Account{DBID: 42, AccessToken: "test", CodexTurnStateTickets: map[string]auth.CodexTurnStateTicket{}}
	store := &auth.Store{}
	store.SetAccountsForTest([]*auth.Account{account})
	h := NewCodexTurnStateHarvester(store, &database.DB{})
	activeCodexTurnStateHarvester.Store(h)
	return h, account
}

func TestCodexTurnStateEchoDoesNotExtendExpiry(t *testing.T) {
	h, account := ticketHarvesterFixture(t)
	state := testTurnStateValue(10)
	original := auth.CodexTurnStateTicket{State: state, Length: len(state), CapturedAt: time.Now().Add(-55 * time.Minute), ExpiresAt: time.Now().Add(5 * time.Minute)}
	account.CodexTurnStateTickets["gpt-test"] = original
	h.recordTicket(account, "gpt-test", original.State, "response", true)
	if got := account.CodexTurnStateTickets["gpt-test"]; !got.ExpiresAt.Equal(original.ExpiresAt) || !got.CapturedAt.Equal(original.CapturedAt) {
		t.Fatal("echo extended ticket lifetime")
	}
	if !h.ticketNeedsRefresh(account, "gpt-test", time.Now(), CurrentCodexTurnStateTicketConfig()) {
		t.Fatal("echo prevented scheduled refresh")
	}
}

func TestCodexTurnStateExpiredLearnedModelRetries(t *testing.T) {
	h, account := ticketHarvesterFixture(t)
	account.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{State: testTurnStateValue(10), Length: len(testTurnStateValue(10)), ExpiresAt: time.Now().Add(-time.Minute)}
	h.refresh(context.Background())
	select {
	case key := <-h.tasks:
		if key.model != "gpt-test" {
			t.Fatalf("unexpected key: %+v", key)
		}
		delete(h.queued, key)
		h.states[key].NextAttempt = time.Now().Add(-time.Second)
	default:
		t.Fatal("expired learned model was not queued")
	}
	h.refresh(context.Background())
	select {
	case <-h.tasks:
	default:
		t.Fatal("model was forgotten after expired ticket pruning")
	}
}

// 响应回购的票据必须继承本次尝试真正使用的绑定出口：出口决策在 ExecuteRequest 的
// 局部 ctx 里定稿，只有逐 attempt 的 CodexTurnStateBinding 能把它带回来。
func TestCodexTurnStateResponseHarvestInheritsBoundEgress(t *testing.T) {
	_, account := ticketHarvesterFixture(t)
	state := testTurnStateValue(10)
	const boundProxy = "http://user:pass@sticky.example:9000/sid-abc123-t"
	ctx := WithCodexTurnStateBinding(BindCodexTurnStateRequest(context.Background(), account, "gpt-test"))
	noteCodexTurnStateEgress(ctx, boundProxy)
	StageCodexTurnStateValue(ctx, state)
	CommitCodexTurnStateRequest(ctx)
	ticket, ok := account.CodexTurnStateTickets["gpt-test"]
	if !ok {
		t.Fatal("bound response ticket was not stored")
	}
	if ticket.State != state || ticket.Source != "response" {
		t.Fatalf("stored ticket = %+v", ticket)
	}
	if ticket.ProxyURL != boundProxy {
		t.Fatalf("harvested ticket lost the bound egress: %q", ticket.ProxyURL)
	}
	if ticket.ProxySID != "abc123" {
		t.Fatalf("harvested ticket sid = %q, want abc123", ticket.ProxySID)
	}
}

// 没走绑定出口的尝试回购到的是无绑定票据，绝不能顶掉同模型上仍然有效的绑定票据。
func TestCodexTurnStateResponseHarvestKeepsExistingBoundTicket(t *testing.T) {
	_, account := ticketHarvesterFixture(t)
	bound := testTurnStateValue(12)
	account.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{
		State: bound, Length: len(bound), CapturedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		ProxyURL: "http://bound.example:9000", ProxySID: "keepme", Source: "probe",
	}
	ctx := WithCodexTurnStateBinding(BindCodexTurnStateRequest(context.Background(), account, "gpt-test"))
	noteCodexTurnStateEgress(ctx, "")
	StageCodexTurnStateValue(ctx, testTurnStateValue(10))
	CommitCodexTurnStateRequest(ctx)
	got, ok := account.CodexTurnStateTickets["gpt-test"]
	if !ok {
		t.Fatal("bound ticket was dropped")
	}
	if got.State != bound || got.ProxyURL != "http://bound.example:9000" || got.ProxySID != "keepme" || got.Source != "probe" {
		t.Fatalf("unbound harvest replaced the bound ticket: %+v", got)
	}
}

// 无绑定出口且没有既有票据时保持原行为：照常保存（无绑定的）回购票据。
func TestCodexTurnStateResponseHarvestStoresWithoutExistingTicket(t *testing.T) {
	_, account := ticketHarvesterFixture(t)
	state := testTurnStateValue(10)
	ctx := WithCodexTurnStateBinding(BindCodexTurnStateRequest(context.Background(), account, "gpt-test"))
	noteCodexTurnStateEgress(ctx, "")
	StageCodexTurnStateValue(ctx, state)
	CommitCodexTurnStateRequest(ctx)
	got, ok := account.CodexTurnStateTickets["gpt-test"]
	if !ok {
		t.Fatal("unbound harvest was not stored")
	}
	if got.State != state || got.ProxyURL != "" || got.Source != "response" {
		t.Fatalf("stored ticket = %+v", got)
	}
}

// 端到端：出站出口由 ExecuteRequest 内部的注入决策定稿，逐 attempt 记录器把
// "这次走了哪条绑定出口"带回 handler，响应回购的票据带着同一份绑定落库。
func TestCodexTurnStateHarvestKeepsBoundEgressEndToEnd(t *testing.T) {
	_, account := ticketHarvesterFixture(t)
	account.AccessToken = "token"
	const boundProxy = "http://sticky.example:9000/sid-mintme"
	injected := testTurnStateValue(10)
	harvested := testTurnStateValue(12)
	account.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{
		State: injected, Length: len(injected), ExpiresAt: time.Now().Add(time.Hour), ProxyURL: boundProxy,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(codexTurnStateHeader); got != injected {
			t.Errorf("outbound turn state = %q, want the bound ticket", got)
		}
		w.Header().Set(codexTurnStateHeader, harvested)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	clientPool.Delete(fmt.Sprintf("resin|%d", account.ID()))

	ctx := WithCodexTurnStateBinding(BindCodexTurnStateRequest(context.Background(), account, "gpt-test"))
	ctx = WithCodexClientModel(ctx, "gpt-test")
	resp, err := ExecuteRequest(ctx, account, []byte(`{"model":"gpt-test","input":"hi"}`), "", "", "api-key-1", nil, http.Header{}, false)
	if err != nil {
		t.Fatalf("ExecuteRequest: %v", err)
	}
	bound, decided := CodexTurnStateBoundEgress(ctx)
	if !decided || bound != boundProxy {
		t.Fatalf("bound egress = %q decided=%v, want %q", bound, decided, boundProxy)
	}
	StageCodexTurnStateResponse(ctx, resp.Header)
	_ = resp.Body.Close()
	CommitCodexTurnStateRequest(ctx)
	ticket, ok := account.CodexTurnStateTickets["gpt-test"]
	if !ok || ticket.State != harvested {
		t.Fatalf("harvested ticket = %+v ok=%v", ticket, ok)
	}
	if ticket.ProxyURL != boundProxy || ticket.ProxySID != "mintme" {
		t.Fatalf("harvest lost the bound egress: proxy=%q sid=%q", ticket.ProxyURL, ticket.ProxySID)
	}
}

func TestCodexTurnStateMissingRequestQueuesWithBackoff(t *testing.T) {
	h, account := ticketHarvesterFixture(t)
	ctx := BindCodexTurnStateRequest(context.Background(), account, "gpt-test")
	key := <-h.tasks
	delete(h.queued, key)
	h.states[key].NextAttempt = time.Now().Add(time.Minute)
	BindCodexTurnStateRequest(context.Background(), account, "gpt-test")
	select {
	case <-h.tasks:
		t.Fatal("request bypassed retry backoff")
	default:
	}
	state := testTurnStateValue(10)
	StageCodexTurnStateValue(ctx, state)
	CommitCodexTurnStateRequest(ctx)
	if account.CodexTurnStateTicketInjection("gpt-test", len(state), time.Now()) == "" {
		t.Fatal("response ticket not published")
	}
	select {
	case id := <-h.persist:
		if id != account.DBID {
			t.Fatal("wrong account persisted")
		}
	default:
		t.Fatal("response ticket not scheduled for persistence")
	}
}

func TestCodexTurnStateMixedTicketLengths(t *testing.T) {
	tickets := []struct {
		length int
		blocks int
	}{
		{len(testTurnStateValue(10)), 10},
		{len(testTurnStateValue(12)), 12},
	}
	for _, configured := range []int{tickets[0].length, tickets[1].length} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			h, account := ticketHarvesterFixture(t)
			cfg := *CurrentCodexTurnStateTicketConfig()
			cfg.TargetLength = configured
			SetCodexTurnStateTicketConfig(&cfg)
			for _, tc := range tickets {
				model := fmt.Sprintf("gpt-test-%d", tc.length)
				state := testTurnStateValue(tc.blocks)
				ctx := BindCodexTurnStateRequest(context.Background(), account, model)
				StageCodexTurnStateValue(ctx, state)
				CommitCodexTurnStateRequest(ctx)
				h.pruneAccountTickets(account, &cfg, time.Now())
				if got := account.CodexTurnStateTicketInjection(model, configured, time.Now()); got != state {
					t.Fatalf("length %d not retained with config %d", tc.length, configured)
				}
				_, _, headers := prepareCodexTurnStateInjection(context.Background(), account, []byte(fmt.Sprintf("{\"model\":\"%s\"}", model)), nil, false)
				if headers.Get(codexTurnStateHeader) != state {
					t.Fatalf("length %d not injected", tc.length)
				}
			}
			if auth.ValidCodexTurnStateTicketValue(testTurnStateValue(11), configured) {
				t.Fatal("unexpected length accepted")
			}
			if auth.ValidCodexTurnStateTicketValue("gAAAAA"+strings.Repeat("x", 292-7)+"\n", configured) {
				t.Fatal("malformed ticket accepted")
			}
		})
	}
}

func TestCodexTurnStateProbeSkipsUnavailableAccounts(t *testing.T) {
	for _, reason := range []string{"quota", "disabled", "model"} {
		t.Run(reason, func(t *testing.T) {
			h, account := ticketHarvesterFixture(t)
			key := codexTurnStateProbeKey{accountID: account.ID(), model: "gpt-test"}
			switch reason {
			case "quota":
				account.UsagePercent7d = 100
				account.UsagePercent7dValid = true
				account.Reset7dAt = time.Now().Add(time.Hour)
			case "disabled":
				account.Disabled = 1
			case "model":
				account.SetModelCooldownUntil(key.model, "rate_limited", time.Now().Add(time.Hour))
			}
			h.enqueueProbe(key, time.Now())
			if len(h.tasks) != 0 {
				t.Fatal("unavailable account queued")
			}
			if TriggerCodexTurnStateProbe(account.ID(), key.model) {
				t.Fatal("manual probe bypassed availability")
			}
			h.runProbeTask(context.Background(), key)
			if h.probed.Load() != 0 {
				t.Fatal("queued probe ran after account became unavailable")
			}
			account.UsagePercent7d = 0
			account.Disabled = 0
			account.ClearModelCooldown(key.model)
			h.enqueueProbe(key, time.Now())
			if len(h.tasks) != 1 {
				t.Fatal("recovered account did not resume probing")
			}
		})
	}
}

func TestCodexTurnStateProbeErrorPreservesOriginal(t *testing.T) {
	original := "CONNECT rejected: 502 Bad Gateway; proxy http://test-user:test-password@proxy.example:3010"
	err := &codexTurnStateStageError{stage: "proxy/upstream_request", err: fmt.Errorf("%s", original)}
	if got := codexTurnStateProbeError(err); got != "proxy/upstream_request: "+original {
		t.Fatalf("error detail changed: %s", got)
	}
	if got := codexTurnStateProbeError(nil); got != "" {
		t.Fatal("nil error must be empty")
	}
}

func TestCodexTurnStateRetryIntervalUsesConfiguredDelay(t *testing.T) {
	for _, seconds := range []int{1, 6, 30, 3600} {
		cfg := &CodexTurnStateTicketConfig{ProbeIntervalSeconds: seconds}
		if got := codexTurnStateRetryInterval(cfg); got != time.Duration(seconds)*time.Second {
			t.Fatalf("delay=%s for config=%d", got, seconds)
		}
	}
	if codexTurnStateRetryInterval(nil) != 6*time.Second {
		t.Fatal("wrong default interval")
	}
}

func TestCodexTurnStateSingleAccountParallelRace(t *testing.T) {
	h, _ := ticketHarvesterFixture(t)
	cfg := *CurrentCodexTurnStateTicketConfig()
	cfg.Concurrency = 5
	SetCodexTurnStateTicketConfig(&cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := make(chan struct{}, 5)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		state, err := h.raceProbes(ctx, 5, func(ctx context.Context) (string, error) {
			started <- struct{}{}
			select {
			case <-release:
				return "winner", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		if err == nil && state != "winner" {
			err = fmt.Errorf("wrong winner")
		}
		done <- err
	}()
	for i := 0; i < 5; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("single account did not reach five concurrent attempts")
		}
	}
	if h.active.Load() != 5 {
		t.Fatalf("active=%d", h.active.Load())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if h.active.Load() != 0 {
		t.Fatal("slots leaked")
	}
}

func TestCodexTurnStateRaceCancelsLosers(t *testing.T) {
	h, _ := ticketHarvesterFixture(t)
	cfg := *CurrentCodexTurnStateTicketConfig()
	cfg.Concurrency = 3
	SetCodexTurnStateTicketConfig(&cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	state, err := h.raceProbes(ctx, 3, func(ctx context.Context) (string, error) { return "winner", nil })
	if err != nil || state != "winner" || h.active.Load() != 0 {
		t.Fatalf("state=%s err=%v active=%d", state, err, h.active.Load())
	}
}

func TestCodexTurnStateProbeRenewsSameTicket(t *testing.T) {
	h, account := ticketHarvesterFixture(t)
	old := time.Now().Add(-time.Minute)
	state := testTurnStateValue(10)
	account.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{State: state, Length: len(state), CapturedAt: old, ExpiresAt: old}
	h.recordTicket(account, "gpt-test", state, "probe", true)
	got := account.CodexTurnStateTickets["gpt-test"]
	if !got.CapturedAt.After(old) || !got.ExpiresAt.After(time.Now()) {
		t.Fatal("same probe ticket not renewed")
	}
	select {
	case <-h.persist:
	default:
		t.Fatal("renewal not persisted")
	}
}

func TestWakeCodexTurnStateHarvesterIsNonBlocking(t *testing.T) {
	h := NewCodexTurnStateHarvester(nil, nil)
	activeCodexTurnStateHarvester.Store(h)
	t.Cleanup(func() { activeCodexTurnStateHarvester.CompareAndSwap(h, nil) })

	WakeCodexTurnStateHarvester()
	select {
	case <-h.wake:
	default:
		t.Fatal("WakeCodexTurnStateHarvester did not deliver a wake signal")
	}

	// A second signal must not block when the harvester has not consumed the
	// first one; the buffered channel coalesces settings updates.
	WakeCodexTurnStateHarvester()
}

func TestCodexTurnStateRefreshDeadlineBeforeScan(t *testing.T) {
	h, account := ticketHarvesterFixture(t)
	now := time.Now()
	cfg := CurrentCodexTurnStateTicketConfig()
	account.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{ExpiresAt: now.Add(610 * time.Second)}
	if got := h.nextRefreshWait(cfg, now, time.Hour); got != 10*time.Second {
		t.Fatalf("wait=%s", got)
	}
	account.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{ExpiresAt: now.Add(599 * time.Second)}
	if got := h.nextRefreshWait(cfg, now, 6*time.Second); got != 6*time.Second {
		t.Fatalf("past deadline caused spin: %s", got)
	}
}

func TestCodexTurnStateRejects356ForConfigured292And332(t *testing.T) {
	for _, length := range []int{292, 332} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			h, account := ticketHarvesterFixture(t)
			cfg := *CurrentCodexTurnStateTicketConfig()
			cfg.TargetLength = length
			SetCodexTurnStateTicketConfig(&cfg)
			bad := testTurnStateValue(13)
			if auth.ValidCodexTurnStateTicketValue(bad, length) {
				t.Fatal("356 accepted")
			}
			h.recordTicket(account, "gpt-test", bad, "probe", true)
			if len(account.CodexTurnStateTickets) != 0 {
				t.Fatal("356 persisted")
			}
			account.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{State: bad, Length: len(bad), ExpiresAt: time.Now().Add(time.Hour)}
			if account.CodexTurnStateTicketInjection("gpt-test", length, time.Now()) != "" {
				t.Fatal("stored 356 injected")
			}
			h.refresh(context.Background())
			if _, ok := account.CodexTurnStateTickets["gpt-test"]; ok {
				t.Fatal("stored 356 not pruned")
			}
			select {
			case <-h.tasks:
			default:
				t.Fatal("replacement not scheduled")
			}
		})
	}
}

func TestCodexTurnStatePerAccountSlots(t *testing.T) {
	h, _ := ticketHarvesterFixture(t)
	cfg := *CurrentCodexTurnStateTicketConfig()
	cfg.Concurrency = 2
	SetCodexTurnStateTicketConfig(&cfg)
	ctx := context.Background()
	for _, id := range []int64{10, 10, 20, 20} {
		if err := h.acquireProbeSlot(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if h.active.Load() != 4 {
		t.Fatal("accounts still share a global limit")
	}
	timeout, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := h.acquireProbeSlot(timeout, 10); err == nil {
		t.Fatal("same account exceeded limit")
	}
	for _, id := range []int64{10, 10, 20, 20} {
		h.releaseProbeSlot(id)
	}
	if h.active.Load() != 0 || len(h.accountActive) != 0 {
		t.Fatal("slots leaked")
	}
}

func TestCodexTurnStateStickyProxyReplacesSID(t *testing.T) {
	template := "http://user-region-Rand-sid-oldvalue-t-120:pass@proxy.example:3010"
	first, firstSID, err := codexTurnStateStickyProxy(template)
	if err != nil || firstSID == "" {
		t.Fatalf("first sticky proxy = %q sid=%q err=%v", first, firstSID, err)
	}
	second, secondSID, err := codexTurnStateStickyProxy(template)
	if err != nil || secondSID == "" {
		t.Fatalf("second sticky proxy = %q sid=%q err=%v", second, secondSID, err)
	}
	if firstSID == secondSID || first == second {
		t.Fatalf("sid was not replaced: %q vs %q", first, second)
	}
	if strings.Contains(first, "oldvalue") || !strings.Contains(first, "-t-120:") {
		t.Fatalf("template shape not preserved: %q", first)
	}
}

func TestCodexTurnStateProbeStopStatus(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden, http.StatusUnauthorized} {
		if !codexTurnStateStopStatus(status) {
			t.Fatalf("status=%d must stop the candidate batch", status)
		}
	}
	for _, status := range []int{http.StatusOK, http.StatusBadGateway, http.StatusServiceUnavailable} {
		if codexTurnStateStopStatus(status) {
			t.Fatalf("status=%d must not stop the candidate batch", status)
		}
	}
}
