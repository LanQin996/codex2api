package proxy

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func newUsageFrameTestAccount() *auth.Account {
	return &auth.Account{
		DBID:       1,
		PlanType:   "plus",
		Status:     auth.StatusReady,
		HealthTier: auth.HealthTierHealthy,
	}
}

func TestApplyCodexRateLimitsFrameUpdatesWindows(t *testing.T) {
	acc := newUsageFrameTestAccount()
	observedAt := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"plan_type":"plus",
		"rate_limits":{
			"primary":{"used_percent":42,"window_minutes":300,"reset_at":1700000900},
			"secondary":{"used_percent":8,"window_minutes":10080,"reset_at":1700604800}
		}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, observedAt)

	pct5h, ok := acc.GetUsagePercent5h()
	if !ok || pct5h != 42 {
		t.Fatalf("5h usage = %v, valid=%v, want 42", pct5h, ok)
	}
	if want := observedAt.Add(15 * time.Minute); !acc.GetReset5hAt().Equal(want) {
		t.Fatalf("Reset5hAt = %v, want %v", acc.GetReset5hAt(), want)
	}
	pct7d, ok := acc.GetUsagePercent7d()
	if !ok || pct7d != 8 {
		t.Fatalf("7d usage = %v, valid=%v, want 8", pct7d, ok)
	}
}

func TestApplyCodexRateLimitsFrameKeepsAbsent5hWindow(t *testing.T) {
	acc := newUsageFrameTestAccount()
	observedAt := time.Unix(1_700_000_000, 0)
	acc.SetUsageSnapshot5hAt(77, observedAt.Add(time.Hour), observedAt)
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"rate_limits":{
			"primary":null,
			"secondary":{"used_percent":12,"window_minutes":10080,"reset_at":1700604800}
		}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, observedAt.Add(time.Minute))

	pct5h, ok := acc.GetUsagePercent5h()
	if !ok || pct5h != 77 {
		t.Fatalf("partial frame cleared 5h snapshot: usage=%v valid=%v", pct5h, ok)
	}
	pct7d, ok := acc.GetUsagePercent7d()
	if !ok || pct7d != 12 {
		t.Fatalf("partial frame dropped its present 7d window: usage=%v valid=%v", pct7d, ok)
	}
}

func TestApplyCodexRateLimitsFrameIgnoresMeteredLimits(t *testing.T) {
	acc := newUsageFrameTestAccount()
	observedAt := time.Unix(1_700_000_000, 0)
	acc.SetUsageSnapshot5hAt(55, observedAt.Add(time.Hour), observedAt)
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"metered_limit_name":"codex_spark",
		"rate_limits":{
			"primary":{"used_percent":99,"window_minutes":300,"reset_at":1700000900}
		}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, observedAt.Add(time.Minute))

	pct5h, ok := acc.GetUsagePercent5h()
	if !ok || pct5h != 55 {
		t.Fatalf("metered limit frame overwrote main 5h snapshot: usage=%v valid=%v", pct5h, ok)
	}
}

func TestApplyCodexResponseMetadataFrameAdoptsUsageHeaders(t *testing.T) {
	acc := newUsageFrameTestAccount()
	observedAt := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{
		"type":"codex.response.metadata",
		"headers":{
			"x-codex-primary-used-percent":"33",
			"x-codex-primary-window-minutes":"300",
			"x-codex-primary-reset-at":"1700000900",
			"x-codex-turn-state":"ts-must-not-be-used",
			"x-codex-safety-buffering-enabled":"true",
			"x-models-etag":"etag-must-not-be-used",
			"authorization":"Bearer must-not-be-used",
			"set-cookie":"must-not-be-used"
		}
	}`)

	headers := codexResponseMetadataHeaders(frame, observedAt)
	if got := headers.Get("x-codex-primary-used-percent"); got != "33" {
		t.Fatalf("quota header = %q, want 33", got)
	}
	for _, name := range []string{
		"x-codex-turn-state",
		"x-codex-safety-buffering-enabled",
		"x-models-etag",
		"authorization",
		"set-cookie",
	} {
		if got := headers.Get(name); got != "" {
			t.Fatalf("%s leaked into synthetic usage headers: %q", name, got)
		}
	}

	ApplyCodexUsageEventFrame(nil, acc, "codex.response.metadata", frame, observedAt)

	pct5h, ok := acc.GetUsagePercent5h()
	if !ok || pct5h != 33 {
		t.Fatalf("5h usage = %v valid=%v, want 33", pct5h, ok)
	}
	if want := observedAt.Add(15 * time.Minute); !acc.GetReset5hAt().Equal(want) {
		t.Fatalf("Reset5hAt = %v, want %v", acc.GetReset5hAt(), want)
	}
}

func TestSyncCodexUsageStateMarks100PercentOnlyWhenPersisting(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	observedAt := time.Now()

	newFullResponse := func() *http.Response {
		resp := &http.Response{Header: http.Header{}}
		resp.Header.Set("x-codex-primary-used-percent", "100")
		resp.Header.Set("x-codex-primary-window-minutes", "300")
		resp.Header.Set("x-codex-primary-reset-after-seconds", "3600")
		resp.Header.Set("x-codex-secondary-used-percent", "100")
		resp.Header.Set("x-codex-secondary-window-minutes", "10080")
		resp.Header.Set("x-codex-secondary-reset-after-seconds", "86400")
		return resp
	}

	throttled := newUsageFrameTestAccount()
	store.AddAccount(throttled)
	result := syncCodexUsageState(store, throttled, newFullResponse(), observedAt, codexUsageSyncOptions{
		persistUsageSnapshot: true,
		shouldPersistUsage:   func(float64, bool, float64, bool) bool { return false },
	})
	if result.Usage7dRateLimited || result.Premium5hRateLimited {
		t.Fatalf("throttled frame marked cooldowns: 7d=%v 5h=%v", result.Usage7dRateLimited, result.Premium5hRateLimited)
	}
	if throttled.Status == auth.StatusCooldown {
		t.Fatal("throttled 100% frame put the account into cooldown")
	}

	persisted := newUsageFrameTestAccount()
	persisted.DBID = 2
	store.AddAccount(persisted)
	result = syncCodexUsageState(store, persisted, newFullResponse(), observedAt, codexUsageSyncOptions{
		persistUsageSnapshot: true,
		shouldPersistUsage:   func(float64, bool, float64, bool) bool { return true },
	})
	if !result.Usage7dRateLimited || !result.Premium5hRateLimited {
		t.Fatalf("persisted 100%% marks = 7d:%v 5h:%v, want both", result.Usage7dRateLimited, result.Premium5hRateLimited)
	}
}

func TestCodexUsageFramesPreserveLongerResponsesCooldown(t *testing.T) {
	for _, idle := range []bool{false, true} {
		for _, kind := range []string{"codex.rate_limits", "codex.response.metadata"} {
			t.Run(fmt.Sprintf("idle=%v/%s", idle, kind), func(t *testing.T) {
				store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
				t.Cleanup(store.Stop)
				acc := newUsageFrameTestAccount()
				until := time.Now().Add(6 * time.Hour)
				acc.Status, acc.CooldownUtil, acc.CooldownReason = auth.StatusCooldown, until, auth.ResponsesRateLimitedCooldownReason
				acc.FailureStreak = 4
				store.AddAccount(acc)
				payload := `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"reset_after_seconds":3600},"secondary":{"used_percent":100,"window_minutes":10080,"reset_after_seconds":7200}}}`
				if kind == "codex.response.metadata" {
					payload = `{"type":"codex.response.metadata","headers":{"x-codex-primary-used-percent":"100","x-codex-primary-window-minutes":"300","x-codex-primary-reset-after-seconds":"3600","x-codex-secondary-used-percent":"100","x-codex-secondary-window-minutes":"10080","x-codex-secondary-reset-after-seconds":"7200"}}`
				}
				if idle {
					ApplyCodexIdleUsageFrame(store, acc, kind, []byte(payload))
				} else {
					ApplyCodexUsageEventFrame(store, acc, kind, gjson.Parse(payload), time.Now())
				}
				if acc.Status != auth.StatusCooldown || !acc.CooldownUtil.Equal(until) || acc.GetCooldownReason() != auth.ResponsesRateLimitedCooldownReason || acc.FailureStreak != 4 {
					t.Fatalf("frame weakened the Responses cooldown: status=%v until=%v reason=%q streak=%d", acc.Status, acc.CooldownUtil, acc.GetCooldownReason(), acc.FailureStreak)
				}
				if pct, ok := acc.GetUsagePercent5h(); !ok || pct != 100 {
					t.Fatalf("5h usage = %v valid=%v, want 100", pct, ok)
				}
				if pct, ok := acc.GetUsagePercent7d(); !ok || pct != 100 {
					t.Fatalf("7d usage = %v valid=%v, want 100", pct, ok)
				}
			})
		}
	}
}

func TestApplyCodexRateLimitsFrameProPrimaryWeekly(t *testing.T) {
	acc := newUsageFrameTestAccount()
	acc.PlanType = "pro"
	now := time.Unix(1_700_000_000, 0)
	acc.SetUsageSnapshot5hAt(12, now.Add(time.Hour), now.Add(-time.Minute))
	// Shape captured from a live CLIProxyAPI fixture: a Pro plan can carry the
	// weekly window in primary and leave secondary null.
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"plan_type":"pro",
		"rate_limits":{
			"allowed":true,
			"limit_reached":false,
			"primary":{"used_percent":81,"window_minutes":10080,"reset_after_seconds":137160,"reset_at":1700137160},
			"secondary":null
		},
		"credits":{"has_credits":false,"unlimited":false,"balance":"0"}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, now)

	if pct, ok := acc.GetUsagePercent7d(); !ok || pct != 81 {
		t.Fatalf("7d usage = %v valid=%v, want 81", pct, ok)
	}
	if pct, ok := acc.GetUsagePercent5h(); !ok || pct != 12 {
		t.Fatalf("primary weekly frame cleared 5h snapshot: usage=%v valid=%v", pct, ok)
	}
	if got, want := acc.GetReset7dAt(), now.Add(137160*time.Second); !got.Equal(want) {
		t.Fatalf("Reset7dAt = %v, want %v", got, want)
	}
}

func TestParseCodexWindowUsageRejectsOutOfRangePercent(t *testing.T) {
	for _, used := range []string{"101", "-1"} {
		if got := parseCodexWindowUsage(used, "300", "60"); got.valid {
			t.Fatalf("used_percent %s accepted: %+v", used, got)
		}
	}
	if got := parseCodexWindowUsage("100", "300", "60"); !got.valid {
		t.Fatal("used_percent 100 rejected")
	}
}

func TestCodexUsageFrameIngestKillSwitch(t *testing.T) {
	SetCodexUsageFrameIngestEnabled(false)
	t.Cleanup(func() { SetCodexUsageFrameIngestEnabled(true) })

	acc := newUsageFrameTestAccount()
	frame := gjson.Parse(`{"rate_limits":{"primary":{"used_percent":42,"window_minutes":300}}}`)
	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, time.Now())
	ApplyCodexIdleUsageFrame(nil, acc, "codex.rate_limits", []byte(`{"rate_limits":{"primary":{"used_percent":42,"window_minutes":300}}}`))

	if pct, ok := acc.GetUsagePercent5h(); ok {
		t.Fatalf("disabled ingestion updated usage to %v", pct)
	}
}

func TestApplyCodexUsageEventFrameRecordsFrameFreshness(t *testing.T) {
	acc := newUsageFrameTestAccount()
	observedAt := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{"rate_limits":{"primary":{"used_percent":42,"window_minutes":300}}}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, observedAt)

	snapshot := acc.GetAccountLiveUsageSnapshot()
	if !snapshot.UsageFrameLastAt.Equal(observedAt) {
		t.Fatalf("frame time = %v, want %v", snapshot.UsageFrameLastAt, observedAt)
	}
	if snapshot.UsageFrameLastKind != "codex.rate_limits" {
		t.Fatalf("frame kind = %q, want codex.rate_limits", snapshot.UsageFrameLastKind)
	}
}

func TestApplyCodexUsageEventFrameIgnoresUnrelatedEvents(t *testing.T) {
	acc := newUsageFrameTestAccount()
	observedAt := time.Unix(1_700_000_000, 0)
	acc.SetUsageSnapshot5hAt(21, observedAt.Add(time.Hour), observedAt)
	frame := gjson.Parse(`{"type":"response.created","response":{"id":"resp-1"}}`)

	ApplyCodexUsageEventFrame(nil, acc, "response.created", frame, observedAt)

	pct5h, ok := acc.GetUsagePercent5h()
	if !ok || pct5h != 21 {
		t.Fatalf("unrelated event changed usage: usage=%v valid=%v", pct5h, ok)
	}
}

func TestApplyCodexRateLimitsFrameFreePlanHasOnlyLongWindow(t *testing.T) {
	acc := newUsageFrameTestAccount()
	acc.PlanType = "free"
	observedAt := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"plan_type":"free",
		"rate_limits":{
			"primary":{"used_percent":64,"window_minutes":10080,"reset_at":1700604800},
			"secondary":null
		}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, observedAt)

	if _, ok := acc.GetUsagePercent5h(); ok {
		t.Fatal("free plan frame must not fabricate a 5h window")
	}
	pct7d, ok := acc.GetUsagePercent7d()
	if !ok || pct7d != 64 {
		t.Fatalf("7d usage = %v valid=%v, want 64", pct7d, ok)
	}
}

func TestApplyCodexRateLimitsFrame5hOnlyKeeps7dSnapshot(t *testing.T) {
	acc := newUsageFrameTestAccount()
	observedAt := time.Unix(1_700_000_000, 0)
	acc.SetUsagePercent7d(31)
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"rate_limits":{
			"primary":{"used_percent":56,"window_minutes":300,"reset_at":1700000900},
			"secondary":null
		}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, observedAt)

	pct5h, ok := acc.GetUsagePercent5h()
	if !ok || pct5h != 56 {
		t.Fatalf("5h usage = %v valid=%v, want 56", pct5h, ok)
	}
	pct7d, ok := acc.GetUsagePercent7d()
	if !ok || pct7d != 31 {
		t.Fatalf("partial 5h frame cleared 7d snapshot: usage=%v valid=%v", pct7d, ok)
	}
}

func TestApplyCodexRateLimitsFrameTeamMonthlyWindow(t *testing.T) {
	acc := newUsageFrameTestAccount()
	acc.PlanType = "team"
	observedAt := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"rate_limits":{
			"primary":{"used_percent":44,"window_minutes":43200,"reset_at":1702592000},
			"secondary":null
		}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, observedAt)

	if _, ok := acc.GetUsagePercent5h(); ok {
		t.Fatal("monthly window must not be classified as 5h")
	}
	pct7d, ok := acc.GetUsagePercent7d()
	if !ok || pct7d != 44 {
		t.Fatalf("7d usage = %v valid=%v, want 44", pct7d, ok)
	}
	if got := acc.GetWindow7dSeconds(); got != 43200*60 {
		t.Fatalf("Window7dSeconds = %d, want %d", got, 43200*60)
	}
}

func TestApplyCodexRateLimitsFramePreservesKnownReset(t *testing.T) {
	acc := newUsageFrameTestAccount()
	observedAt := time.Unix(1_700_000_000, 0)
	resetAt := observedAt.Add(2 * time.Hour)
	acc.SetUsageSnapshot5hAt(20, resetAt, observedAt)
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"rate_limits":{
			"primary":{"used_percent":70,"window_minutes":300},
			"secondary":null
		}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, observedAt.Add(time.Minute))

	pct5h, ok := acc.GetUsagePercent5h()
	if !ok || pct5h != 70 {
		t.Fatalf("5h usage = %v valid=%v, want 70", pct5h, ok)
	}
	if !acc.GetReset5hAt().Equal(resetAt) {
		t.Fatalf("Reset5hAt = %v, want preserved %v", acc.GetReset5hAt(), resetAt)
	}
}

func TestApplyCodexIdleUsageFrameWithoutStoreUpdatesRuntime(t *testing.T) {
	acc := newUsageFrameTestAccount()
	now := time.Now()
	acc.SetUsageSnapshot5hAt(20, now.Add(time.Hour), now)
	acc.SetUsagePercent7d(10)

	ApplyCodexIdleUsageFrame(nil, acc, "codex.rate_limits", []byte(`{
		"type":"codex.rate_limits",
		"rate_limits":{
			"primary":{"used_percent":75,"window_minutes":300,"reset_at":1700000900},
			"secondary":null
		}
	}`))

	pct5h, ok := acc.GetUsagePercent5h()
	if !ok || pct5h != 75 {
		t.Fatalf("idle frame 5h usage = %v valid=%v, want 75", pct5h, ok)
	}
	pct7d, ok := acc.GetUsagePercent7d()
	if !ok || pct7d != 10 {
		t.Fatalf("idle partial frame cleared 7d snapshot: usage=%v valid=%v", pct7d, ok)
	}
}

func TestParseCodexUsageHeaderObservationAcceptsResetAt(t *testing.T) {
	now := time.Now()
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("x-codex-primary-used-percent", "33")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-at", strconv.FormatInt(now.Add(15*time.Minute).Unix(), 10))

	observation := parseCodexUsageHeaderObservation(resp)
	if !observation.w5h.valid {
		t.Fatal("reset-at header did not produce a valid 5h window")
	}
	if observation.w5h.resetSec < 14*60 || observation.w5h.resetSec > 16*60 {
		t.Fatalf("resetSec = %v, want about 900", observation.w5h.resetSec)
	}
}

func TestApplyCodexRateLimitsFramePreservesWorkspaceHardStop(t *testing.T) {
	acc := newUsageFrameTestAccount()
	acc.SetRateLimitReachedType("workspace_member_credits_depleted")
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"plan_type":"plus",
		"rate_limits":{
			"primary":{"used_percent":42,"window_minutes":300,"reset_at":1700000900},
			"secondary":{"used_percent":8,"window_minutes":10080,"reset_at":1700604800}
		},
		"credits":{"has_credits":true,"unlimited":false,"balance":"10"}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, time.Unix(1_700_000_000, 0))

	if got := acc.GetRateLimitReachedType(); got != "workspace_member_credits_depleted" {
		t.Fatalf("frame cleared workspace hard stop: %q", got)
	}
}

func TestApplyCodexMetadataFramePreservesWorkspaceHardStop(t *testing.T) {
	acc := newUsageFrameTestAccount()
	acc.SetRateLimitReachedType("workspace_member_credits_depleted")
	frame := gjson.Parse(`{
		"type":"codex.response.metadata",
		"headers":{"x-codex-primary-used-percent":"33","x-codex-primary-window-minutes":"300"}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.response.metadata", frame, time.Unix(1_700_000_000, 0))

	if got := acc.GetRateLimitReachedType(); got != "workspace_member_credits_depleted" {
		t.Fatalf("metadata frame cleared workspace hard stop: %q", got)
	}
}

func TestCodexResponseMetadataHeadersUnwrapsArrayValues(t *testing.T) {
	observedAt := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{"headers":{
		"x-codex-primary-used-percent":["42"],
		"x-codex-primary-window-minutes":["300"],
		"x-codex-primary-reset-at":[1700000900],
		"x-codex-credits-has-credits":["true"],
		"x-codex-turn-state":["ts-must-not-be-used"]
	}}`)

	headers := codexResponseMetadataHeaders(frame, observedAt)
	if got := headers.Get("x-codex-primary-used-percent"); got != "42" {
		t.Fatalf("array header = %q, want 42", got)
	}
	if got := headers.Get("x-codex-primary-reset-after-seconds"); got != "900" {
		t.Fatalf("array reset = %q, want 900", got)
	}
	if got := headers.Get("x-codex-credits-has-credits"); got != "true" {
		t.Fatalf("array credits = %q, want true", got)
	}
	if got := headers.Get("x-codex-turn-state"); got != "" {
		t.Fatalf("turn state leaked into quota headers: %q", got)
	}
}

func TestCodexFrameRejectsControlCharacters(t *testing.T) {
	observedAt := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{
		"plan_type":"plus\r\nX-Injected: 1",
		"rate_limits":{"primary":{"used_percent":42,"window_minutes":300}},
		"credits":{"has_credits":true,"unlimited":false,"balance":"10\n20"}
	}`)

	headers := codexRateLimitsHeaders(frame, observedAt)
	if got := headers.Get("x-codex-plan-type"); got != "" {
		t.Fatalf("control characters in plan type were accepted: %q", got)
	}
	if got := headers.Get("x-codex-credits-balance"); got != "" {
		t.Fatalf("control characters in balance were accepted: %q", got)
	}
	if got := headers.Get("x-codex-primary-used-percent"); got != "42" {
		t.Fatalf("valid window was discarded: %q", got)
	}

	metadata := codexResponseMetadataHeaders(gjson.Parse(`{"headers":{
		"x-codex-plan-type":"plus\r\nX-Injected: 1"
	}}`), observedAt)
	if got := metadata.Get("x-codex-plan-type"); got != "" {
		t.Fatalf("control characters in metadata header were accepted: %q", got)
	}
}

func TestCodexRateLimitsFrameTreatsZeroRelativeResetAsNow(t *testing.T) {
	observedAt := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{"rate_limits":{"primary":{
		"used_percent":50,"window_minutes":300,"resets_in_seconds":0
	}}}`)

	headers := codexRateLimitsHeaders(frame, observedAt)
	if got := headers.Get("x-codex-primary-reset-after-seconds"); got != "0" {
		t.Fatalf("relative zero reset = %q, want 0", got)
	}
}

func TestCodexFrameResetRejectsNonIntegerNumbers(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	for _, raw := range []string{"1.5", "1e3", "9223372036854775808", "-9223372036854775809"} {
		t.Run(raw, func(t *testing.T) {
			for _, field := range []string{"reset_after_seconds", "reset_at"} {
				window := gjson.Parse(`{"` + field + `":` + raw + `}`)
				if got := codexFrameResetSeconds(window, at); got != "" {
					t.Errorf("%s=%s accepted as reset %q", field, raw, got)
				}
			}
			for _, field := range []string{"reset-at", "reset-after-seconds"} {
				metadata := codexResponseMetadataHeaders(gjson.Parse(`{"headers":{"x-codex-primary-`+field+`":`+raw+`}}`), at)
				if got := metadata.Get("x-codex-primary-reset-after-seconds"); got != "" {
					t.Errorf("metadata %s=%s accepted as reset %q", field, raw, got)
				}
			}
		})
	}
}

func TestCodexFrameResetRejectsDurationOverflow(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	for _, test := range []struct {
		field, header, raw string
	}{
		{"reset_after_seconds", "reset-after-seconds", "9223372037"},
		{"reset_at", "reset-at", "10923372037"},
	} {
		if got := codexFrameResetSeconds(gjson.Parse(`{"`+test.field+`":`+test.raw+`}`), at); got != "" {
			t.Errorf("duration-overflow %s accepted: %q", test.field, got)
		}
		headers := codexResponseMetadataHeaders(gjson.Parse(`{"headers":{"x-codex-primary-`+test.header+`":`+test.raw+`}}`), at)
		if got := headers.Get("x-codex-primary-reset-after-seconds"); got != "" {
			t.Errorf("metadata duration-overflow %s accepted: %q", test.header, got)
		}
	}
}

func TestApplyCodexRateLimitsFrameIgnoresGPTCodexMeteredLimit(t *testing.T) {
	acc := newUsageFrameTestAccount()
	now := time.Now()
	acc.SetUsageSnapshot5hAt(55, now.Add(time.Hour), now)
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"limit_name":"gpt-codex",
		"rate_limits":{"primary":{"used_percent":99,"window_minutes":300,"reset_at":1700000900}}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, now)

	pct, ok := acc.GetUsagePercent5h()
	if !ok || pct != 55 {
		t.Fatalf("metered gpt-codex frame overwrote main window: usage=%v valid=%v", pct, ok)
	}
}

func TestApplyCodexRateLimitsFrameAcceptsDefaultLimitName(t *testing.T) {
	acc := newUsageFrameTestAccount()
	now := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"limit_name":"codex",
		"rate_limits":{"primary":{"used_percent":42,"window_minutes":300}}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, now)

	if pct, ok := acc.GetUsagePercent5h(); !ok || pct != 42 {
		t.Fatalf("default limit_name frame rejected: usage=%v valid=%v", pct, ok)
	}
}

func TestApplyCodexUsageHeaderObservationKeepsHigherUsageWithinSameWindow(t *testing.T) {
	acc := newUsageFrameTestAccount()
	t0 := time.Unix(1_700_000_000, 0)
	first := codexUsageHeaderObservation{
		w5h:           codexWindowUsage{usedPct: 50, resetSec: 3600, windowMin: 300, valid: true, hasReset: true},
		authoritative: true,
	}
	applyCodexUsageHeaderObservation(nil, acc, first, t0, false)

	second := first
	second.w5h.usedPct = 40
	second.w5h.resetSec = 3590 // t0+10s+3590s is the same absolute reset.
	applyCodexUsageHeaderObservation(nil, acc, second, t0.Add(10*time.Second), false)
	if pct, _ := acc.GetUsagePercent5h(); pct != 50 {
		t.Fatalf("out-of-order frame rolled 5h usage back to %v, want 50", pct)
	}

	third := first
	third.w5h.usedPct = 10
	third.w5h.resetSec = 18000 // next window.
	applyCodexUsageHeaderObservation(nil, acc, third, t0.Add(20*time.Second), false)
	if pct, _ := acc.GetUsagePercent5h(); pct != 10 {
		t.Fatalf("new window usage = %v, want 10", pct)
	}
}

func TestApplyCodexUsageHeaderObservationBoundsMissingReset(t *testing.T) {
	acc := newUsageFrameTestAccount()
	t0 := time.Unix(1_700_000_000, 0)

	applyCodexUsageHeaderObservation(nil, acc, codexUsageHeaderObservation{
		w5h:           codexWindowUsage{usedPct: 80, windowMin: 300, valid: true},
		authoritative: true,
	}, t0, false)
	if got, want := acc.GetReset5hAt(), t0.Add(5*time.Hour); !got.Equal(want) {
		t.Fatalf("5h reset fallback = %v, want %v", got, want)
	}

	applyCodexUsageHeaderObservation(nil, acc, codexUsageHeaderObservation{
		w7d:           codexWindowUsage{usedPct: 90, windowMin: 10080, valid: true},
		authoritative: true,
	}, t0, false)
	if got, want := acc.GetReset7dAt(), t0.Add(7*24*time.Hour); !got.Equal(want) {
		t.Fatalf("7d reset fallback = %v, want %v", got, want)
	}
}

func TestApplyCodexUsageHeaderObservationStaleResetStartsNewWindow(t *testing.T) {
	acc := newUsageFrameTestAccount()
	t0 := time.Unix(1_700_000_000, 0)
	// The stored window has already reset, but the new frame omits reset_at.
	acc.SetUsageSnapshot5hAt(100, t0.Add(-time.Minute), t0.Add(-time.Hour))
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(t0.Add(-time.Minute))

	applyCodexUsageHeaderObservation(nil, acc, codexUsageHeaderObservation{
		w5h:           codexWindowUsage{usedPct: 5, windowMin: 300, valid: true},
		w7d:           codexWindowUsage{usedPct: 6, windowMin: 10080, valid: true},
		authoritative: true,
	}, t0, false)

	if pct, ok := acc.GetUsagePercent5h(); !ok || pct != 5 {
		t.Fatalf("5h usage = %v valid=%v, want 5 after the window rolled over", pct, ok)
	}
	if want := t0.Add(5 * time.Hour); !acc.GetReset5hAt().Equal(want) {
		t.Fatalf("5h reset = %v, want %v", acc.GetReset5hAt(), want)
	}
	if pct, ok := acc.GetUsagePercent7d(); !ok || pct != 6 {
		t.Fatalf("7d usage = %v valid=%v, want 6 after the window rolled over", pct, ok)
	}
	if want := t0.Add(7 * 24 * time.Hour); !acc.GetReset7dAt().Equal(want) {
		t.Fatalf("7d reset = %v, want %v", acc.GetReset7dAt(), want)
	}
}

func TestCodexResetPrefersAbsoluteOverZeroRelative(t *testing.T) {
	observedAt := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{"rate_limits":{"primary":{
		"used_percent":50,"window_minutes":300,
		"resets_in_seconds":0,"reset_at":1700003600
	}}}`)

	headers := codexRateLimitsHeaders(frame, observedAt)
	if got := headers.Get("x-codex-primary-reset-after-seconds"); got != "3600" {
		t.Fatalf("reset = %q, want the future absolute reset 3600", got)
	}
}

func TestParseCodexWindowUsageRejectsTrailingGarbage(t *testing.T) {
	if got := parseCodexWindowUsage("42abc", "300", "60"); got.valid {
		t.Fatalf("trailing garbage in used_percent accepted: %+v", got)
	}
	if got := parseCodexWindowUsage("42", "300x", "60"); got.valid {
		t.Fatalf("trailing garbage in window_minutes accepted: %+v", got)
	}
	if got := parseCodexWindowUsage("42", "300", "60abc"); !got.valid {
		t.Fatal("valid window rejected because the reset value is malformed")
	} else if got.hasReset {
		t.Fatalf("malformed reset accepted as a reset value: %+v", got)
	}
}

func TestCodexRateLimitsFrameUsesLimitIDWhenDisplayNameDiffers(t *testing.T) {
	acc := newUsageFrameTestAccount()
	now := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{
		"type":"codex.rate_limits",
		"limit_id":"codex",
		"limit_name":"gpt-5.2-codex-sonic",
		"rate_limits":{"primary":{"used_percent":42,"window_minutes":300}}
	}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, now)

	if pct, ok := acc.GetUsagePercent5h(); !ok || pct != 42 {
		t.Fatalf("main limit frame rejected: usage=%v valid=%v", pct, ok)
	}
}

func TestApplyCodexIdleUsageFrameAppliesCredits(t *testing.T) {
	acc := newUsageFrameTestAccount()
	acc.ApplySparseCreditsHeaders(true, false, nil, nil, "workspace_member_credits_depleted")

	ApplyCodexIdleUsageFrame(nil, acc, "codex.rate_limits", []byte(`{
		"type":"codex.rate_limits",
		"credits":{"has_credits":false,"unlimited":false,"balance":"0"}
	}`))

	credits, ok := acc.GetCreditBalance()
	if !ok || credits.HasCredits {
		t.Fatalf("idle frame did not clear the credits flag: %+v valid=%v", credits, ok)
	}
	if got := acc.GetRateLimitReachedType(); got != "workspace_member_credits_depleted" {
		t.Fatalf("idle credits frame cleared the workspace hard stop: %q", got)
	}
}

func TestCodexRateLimitsFrameIgnoresSingleCreditsFlag(t *testing.T) {
	observedAt := time.Unix(1_700_000_000, 0)
	frame := gjson.Parse(`{"credits":{"has_credits":true}}`)

	headers := codexRateLimitsHeaders(frame, observedAt)
	if got := headers.Get("x-codex-credits-has-credits"); got != "" {
		t.Fatalf("single credits flag was adopted: %q", got)
	}
}

func TestCodexUsageFramesSkipRelayAccounts(t *testing.T) {
	acc := newUsageFrameTestAccount()
	acc.UpstreamType = auth.UpstreamOpenAIResponses
	acc.BaseURL = "relay.example"
	acc.APIKey = "relay-key"
	frame := gjson.Parse(`{"rate_limits":{"primary":{"used_percent":42,"window_minutes":300}}}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", frame, time.Now())
	ApplyCodexIdleUsageFrame(nil, acc, "codex.rate_limits", []byte(`{"rate_limits":{"primary":{"used_percent":42,"window_minutes":300}}}`))

	if pct, ok := acc.GetUsagePercent5h(); ok {
		t.Fatalf("relay account ingested a forwarded frame: usage=%v", pct)
	}
}

func TestApplyCodexUsageEventFrameIgnoresOlderObservation(t *testing.T) {
	acc := newUsageFrameTestAccount()
	t2 := time.Unix(1_700_000_000, 0)
	t1 := t2.Add(-time.Minute)
	newer := gjson.Parse(`{"rate_limits":{"primary":{"used_percent":60,"window_minutes":300}}}`)
	older := gjson.Parse(`{"rate_limits":{"primary":{"used_percent":30,"window_minutes":300}}}`)

	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", newer, t2)
	ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", older, t1)

	if pct, _ := acc.GetUsagePercent5h(); pct != 60 {
		t.Fatalf("older observation rolled usage back to %v, want 60", pct)
	}
}

func TestCodexUsageFramesOrderCreditsAndFreshnessWithUsage(t *testing.T) {
	for _, creditsOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("creditsOnly=%v", creditsOnly), func(t *testing.T) {
			acc := newUsageFrameTestAccount()
			at := time.Now()
			newer := gjson.Parse(`{"rate_limits":{"primary":{"used_percent":60,"window_minutes":300}},"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`)
			older := `{"rate_limits":{"primary":{"used_percent":10,"window_minutes":300}},"credits":{"has_credits":true,"unlimited":false,"balance":"100"}}`
			if creditsOnly {
				older = `{"credits":{"has_credits":true,"unlimited":false,"balance":"100"}}`
			}
			ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", newer, at)
			ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", gjson.Parse(older), at.Add(-time.Second))
			credits, valid := acc.GetCreditBalance()
			snap := acc.GetAccountLiveUsageSnapshot()
			if !valid || credits.HasCredits || snap.UsagePercent5h != 60 || !snap.UsageFrameLastAt.Equal(at) {
				t.Fatalf("older frame applied: credits=%+v valid=%v snapshot=%+v", credits, valid, snap)
			}
			ApplyCodexUsageEventFrame(nil, acc, "codex.rate_limits", gjson.Parse(`{"credits":{"has_credits":true,"unlimited":false,"balance":"100"}}`), at.Add(time.Second))
			credits, valid = acc.GetCreditBalance()
			if !valid || !credits.HasCredits || !acc.GetAccountLiveUsageSnapshot().UsageFrameLastAt.Equal(at.Add(time.Second)) {
				t.Fatal("newer credits-only observation was not applied")
			}
		})
	}
}

func TestCodexUsageFramePersistenceRespectsThrottle(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "usage-frame.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	accountID, err := db.InsertAccountWithCredentials(ctx, "frame-test", map[string]any{
		"access_token": "at-test",
		"plan_type":    "plus",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	account := newUsageFrameTestAccount()
	account.DBID = accountID
	account.AccessToken = "at-test"
	store.AddAccount(account)

	previous := codexUsageFramePersistInterval()
	SetCodexUsageFramePersistInterval(time.Hour)
	t.Cleanup(func() { SetCodexUsageFramePersistInterval(previous) })

	t0 := time.Now()
	apply := func(pct float64, at time.Time) {
		frame := gjson.Parse(fmt.Sprintf(
			`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":%v,"window_minutes":300,"reset_at":%d}}}`,
			pct, at.Add(3*time.Hour).Unix(),
		))
		ApplyCodexUsageEventFrame(store, account, "codex.rate_limits", frame, at)
	}

	apply(10, t0)
	waitPersisted5h(t, dbPath, accountID, 10)
	waitUsageFrameThrottled(t, account)

	apply(12, t0.Add(time.Second))
	if pct, _ := account.GetUsagePercent5h(); pct != 12 {
		t.Fatalf("throttled frame did not update memory: %v", pct)
	}
	time.Sleep(150 * time.Millisecond)
	if got := readPersisted5h(t, dbPath, accountID); got != 10 {
		t.Fatalf("throttled frame wrote %v, want the previous 10", got)
	}

	apply(100, t0.Add(2*time.Second))
	waitPersisted5h(t, dbPath, accountID, 100)
}

// TestCodexUsageFramePersistDoesNotBlockOnLockedDB verifies the read path only
// enqueues the snapshot; a database write lock must not stall the caller.
func TestCodexUsageFramePersistDoesNotBlockOnLockedDB(t *testing.T) {
	t.Run("usage", func(t *testing.T) {
		testCodexFrameOnLockedDB(t, `{"rate_limits":{"primary":{"used_percent":42,"window_minutes":300}}}`, 42)
	})
	t.Run("exhaustion_credits_and_plan", func(t *testing.T) {
		testCodexFrameOnLockedDB(t, `{"plan_type":"pro","rate_limits":{"primary":{"used_percent":100,"window_minutes":300},"secondary":{"used_percent":100,"window_minutes":10080}},"credits":{"has_credits":true,"unlimited":false,"balance":"10"}}`, 100)
	})
	t.Run("credits_only", func(t *testing.T) {
		testCodexFrameOnLockedDB(t, `{"credits":{"has_credits":true,"unlimited":false,"balance":"10"}}`, -1)
	})
}

func testCodexFrameOnLockedDB(t *testing.T, payload string, wantUsage float64) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "usage-frame-locked.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	accountID, err := db.InsertAccountWithCredentials(ctx, "frame-locked", map[string]any{
		"access_token": "at-test", "plan_type": "plus",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	account := newUsageFrameTestAccount()
	account.DBID = accountID
	account.AccessToken = "at-test"
	store.AddAccount(account)

	lockConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = lockConn.Close() })
	conn, err := lockConn.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}

	frame := gjson.Parse(payload)
	done := make(chan struct{})
	go func() {
		ApplyCodexUsageEventFrame(store, account, "codex.rate_limits", frame, time.Now())
		close(done)
	}()
	returned := false
	select {
	case <-done:
		returned = true
	case <-time.After(500 * time.Millisecond):
		t.Error("frame path waited on a locked database")
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !returned {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("frame did not finish after the DB lock was released")
		}
	}
	if wantUsage >= 0 {
		if pct, ok := account.GetUsagePercent5h(); !ok || pct != wantUsage {
			t.Fatalf("memory usage = %v valid=%v, want %v", pct, ok, wantUsage)
		}
		waitPersisted5h(t, dbPath, accountID, wantUsage)
	} else {
		deadline := time.Now().Add(3 * time.Second)
		for {
			row, err := db.GetAccountByID(ctx, accountID)
			if err != nil {
				t.Fatal(err)
			}
			if row.GetCredential("codex_credits") != "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("credits-only frame was not persisted")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func waitUsageFrameThrottled(t *testing.T, account *auth.Account) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for account.ShouldPersistUsageFrame(time.Now(), 10, true, 0, false, time.Hour) {
		if time.Now().After(deadline) {
			t.Fatal("usage frame throttle did not advance after the async write")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitPersisted5h(t *testing.T, dbPath string, accountID int64, want float64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := readPersisted5h(t, dbPath, accountID); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted 5h usage = %v, want %v", readPersisted5h(t, dbPath, accountID), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readPersisted5h(t *testing.T, dbPath string, accountID int64) float64 {
	t.Helper()
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = conn.Close() }()
	var raw string
	if err := conn.QueryRow(`SELECT credentials FROM accounts WHERE id = $1`, accountID).Scan(&raw); err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	return gjson.Get(raw, "codex_5h_used_percent").Float()
}
