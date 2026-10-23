package proxy

import (
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func admissionAllow(l *apiKeyAdmissionLimiter, keyID int64, rpm, rpd, requests int, now time.Time) bool {
	ok, _ := l.allow(keyID, rpm, rpd, requests, now)
	return ok
}

func TestAPIKeyAdmissionLimiterRejectsBurst(t *testing.T) {
	limiter := newAPIKeyAdmissionLimiter()
	now := time.Unix(1_700_000_000, 0)

	for i := 0; i < 2; i++ {
		if !admissionAllow(limiter, 1, 2, 0, 1, now) {
			t.Fatalf("request %d within RPM rejected", i+1)
		}
	}
	if admissionAllow(limiter, 1, 2, 0, 1, now) {
		t.Fatal("third request in the same second should exceed RPM=2")
	}
	if !admissionAllow(limiter, 2, 2, 0, 1, now) {
		t.Fatal("other API key should not be affected")
	}
	if !admissionAllow(limiter, 1, 2, 0, 1, now.Add(time.Minute)) {
		t.Fatal("request should be admitted after the RPM window rolls")
	}
}

func TestAPIKeyAdmissionLimiterConcurrentBurst(t *testing.T) {
	limiter := newAPIKeyAdmissionLimiter()
	now := time.Unix(1_700_000_000, 0)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if admissionAllow(limiter, 1, 5, 0, 1, now) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 5 {
		t.Fatalf("allowed = %d, want exactly 5 (RPM limit)", got)
	}
}

func TestAPIKeyAdmissionLimiterReservesBatchRequests(t *testing.T) {
	limiter := newAPIKeyAdmissionLimiter()
	now := time.Unix(1_700_000_000, 0)
	if !admissionAllow(limiter, 1, 10, 0, 4, now) {
		t.Fatal("batch of 4 within RPM=10 rejected")
	}
	if admissionAllow(limiter, 1, 10, 0, 7, now) {
		t.Fatal("batch of 7 would exceed the remaining RPM budget")
	}
	if !admissionAllow(limiter, 1, 10, 0, 6, now) {
		t.Fatal("batch of 6 should fit exactly")
	}
}

func TestAPIKeyAdmissionLimiterEnforcesRPD(t *testing.T) {
	limiter := newAPIKeyAdmissionLimiter()
	now := time.Unix(1_700_000_000, 0)
	if !admissionAllow(limiter, 1, 0, 2, 1, now) {
		t.Fatal("first RPD request rejected")
	}
	if !admissionAllow(limiter, 1, 0, 2, 1, now.Add(time.Hour)) {
		t.Fatal("second RPD request rejected")
	}
	if ok, reason := limiter.allow(1, 0, 2, 1, now.Add(2*time.Hour)); ok || reason != "rpd" {
		t.Fatalf("third RPD request = (%v, %q), want (false, rpd)", ok, reason)
	}
	if !admissionAllow(limiter, 1, 0, 2, 1, now.Add(apiKeyRPDWindow)) {
		t.Fatal("RPD window should reset after 24h")
	}
}

func TestAPIKeyAdmissionLimiterRPDDoesNotResetRecentUsage(t *testing.T) {
	limiter := newAPIKeyAdmissionLimiter()
	now := time.Unix(1_700_000_000, 0)
	admissionAllow(limiter, 1, 0, 2, 1, now)
	admissionAllow(limiter, 1, 0, 2, 1, now.Add(23*time.Hour))
	if !admissionAllow(limiter, 1, 0, 2, 1, now.Add(24*time.Hour)) {
		t.Fatal("oldest request should have expired")
	}
	if admissionAllow(limiter, 1, 0, 2, 1, now.Add(24*time.Hour)) {
		t.Fatal("23h request must still count in the sliding 24h window")
	}
}

func TestAPIKeyAdmissionLimiterPreservesLiveCountersWhenFull(t *testing.T) {
	limiter := newAPIKeyAdmissionLimiter()
	now := time.Unix(1_700_000_000, 0)
	for i := 1; i <= apiKeyAdmissionMaxKeys; i++ {
		if !admissionAllow(limiter, int64(i), 1, 0, 1, now) {
			t.Fatalf("key %d rejected while filling the map", i)
		}
	}
	newKey := int64(apiKeyAdmissionMaxKeys + 1)
	if ok, reason := limiter.allow(newKey, 1, 0, 1, now); ok || reason != "capacity" {
		t.Fatalf("full table admission=(%v,%q), want capacity rejection", ok, reason)
	}
	if admissionAllow(limiter, 1, 1, 0, 1, now) {
		t.Fatal("existing live counter was evicted or reset")
	}
	if !admissionAllow(limiter, newKey, 1, 0, 1, now.Add(apiKeyAdmissionIdleTTL+time.Second)) {
		t.Fatal("expired entries should make room for a new key")
	}
	if got := len(limiter.entries); got > apiKeyAdmissionMaxKeys {
		t.Fatalf("entries = %d, want <= %d", got, apiKeyAdmissionMaxKeys)
	}
}

func TestAPIKeyAdmissionLimiterCoalescesRPDConservatively(t *testing.T) {
	l := newAPIKeyAdmissionLimiter()
	now := time.Unix(1_700_000_040, 0) // start of a minute
	admissionAllow(l, 1, 0, 2, 1, now)
	admissionAllow(l, 1, 0, 2, 1, now.Add(59*time.Second))
	if len(l.entries[1].rpdBuckets) != 1 {
		t.Fatal("same-minute requests should share a bucket")
	}
	if admissionAllow(l, 1, 0, 2, 1, now.Add(24*time.Hour)) {
		t.Fatal("must not expire the last request in a coalesced bucket early")
	}
	if !admissionAllow(l, 1, 0, 2, 1, now.Add(24*time.Hour+59*time.Second)) {
		t.Fatal("coalesced bucket should expire after its last request ages out")
	}
}

func TestBatchAdmissionDoesNotReserveSlotsTwice(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admission.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := &Handler{db: db}
	limits := database.APIKeyLimits{RPM: 3}
	c := newImageLimitCtx(t, "/v1/images/generations", `{"prompt":"cat"}`, limits)

	// The enqueue check must not consume RPM; each upstream call reserves one.
	if status, msg := h.EnforceAPIKeyLimitsForRequests(c, "gpt-image-2", 3); status != 0 {
		t.Fatalf("batch admission = %d (%s), want pass", status, msg)
	}
	if _, ok := h.admissionLimiter().entries[1]; ok {
		t.Fatal("batch admission reserved RPM slots before execution")
	}
	for i := 0; i < 3; i++ {
		if status, msg := h.EnforceAPIKeyLimits(c, "gpt-image-2"); status != 0 {
			t.Fatalf("per-call admission %d = %d (%s), want pass", i+1, status, msg)
		}
	}
	if status, _ := h.EnforceAPIKeyLimits(c, "gpt-image-2"); status != http.StatusTooManyRequests {
		t.Fatalf("fourth per-call admission = %d, want 429", status)
	}
}
