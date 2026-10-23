package auth

import (
	"testing"
	"time"
)

func TestShouldPersistUsageFrameThrottlesAndForces(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	now := time.Unix(1_700_000_000, 0)
	interval := time.Minute

	if !acc.ShouldPersistUsageFrame(now, 10, true, 5, true, interval) {
		t.Fatal("first observation must persist")
	}
	acc.MarkUsageFramePersisted(now, 10, true, 5, true)
	if acc.ShouldPersistUsageFrame(now.Add(time.Second), 10.5, true, 5, true, interval) {
		t.Fatal("small delta inside the interval must be throttled")
	}
	if !acc.ShouldPersistUsageFrame(now.Add(time.Second), 16, true, 5, true, interval) {
		t.Fatal("a 5pp delta must force persistence")
	}
	acc.MarkUsageFramePersisted(now.Add(time.Second), 16, true, 5, true)
	if acc.ShouldPersistUsageFrame(now.Add(2*time.Second), 16.5, true, 5, true, interval) {
		t.Fatal("small delta after a forced write must be throttled again")
	}
	if !acc.ShouldPersistUsageFrame(now.Add(2*time.Minute), 16.5, true, 5, true, interval) {
		t.Fatal("elapsed interval must persist")
	}
}

func TestShouldPersistUsageFrameRetriesUntilMarked(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	now := time.Unix(1_700_000_000, 0)

	if !acc.ShouldPersistUsageFrame(now, 10, true, 5, true, time.Minute) {
		t.Fatal("first observation must persist")
	}
	// Simulate a failed write: without MarkUsageFramePersisted the decision
	// must be retried instead of being throttled for the interval.
	if !acc.ShouldPersistUsageFrame(now.Add(time.Second), 10, true, 5, true, time.Minute) {
		t.Fatal("failed write must be retried on the next observation")
	}
}

func TestShouldPersistUsageFrameForcesThresholdCrossing(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	now := time.Unix(1_700_000_000, 0)
	acc.effectiveAutoPause5h = 0.5

	if !acc.ShouldPersistUsageFrame(now, 48, true, 0, false, time.Hour) {
		t.Fatal("first observation must persist")
	}
	acc.MarkUsageFramePersisted(now, 48, true, 0, false)
	if !acc.ShouldPersistUsageFrame(now.Add(time.Second), 50, true, 0, false, time.Hour) {
		t.Fatal("crossing the auto-pause threshold must force persistence")
	}
}

func TestGetAccountLiveUsageSnapshotProjectsStatusAndUsage(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.9
	acc.UsagePercent5h = 95
	acc.UsagePercent5hValid = true
	acc.Reset5hAt = time.Now().Add(time.Hour)
	acc.UsagePercent7d = 31
	acc.UsagePercent7dValid = true
	acc.UsageUpdatedAt = time.Now()
	setAutoPauseThresholdsWithGuard(acc, 5)

	snapshot := acc.GetAccountLiveUsageSnapshot()
	if snapshot.Status != "quota_paused" {
		t.Fatalf("status = %q, want quota_paused", snapshot.Status)
	}
	if !snapshot.UsagePercent5hValid || snapshot.UsagePercent5h != 95 {
		t.Fatalf("5h snapshot = %+v", snapshot)
	}
	if !snapshot.UsagePercent7dValid || snapshot.UsagePercent7d != 31 {
		t.Fatalf("7d snapshot = %+v", snapshot)
	}
}

func TestTryApplyUsageObservationSkipsWhileLocked(t *testing.T) {
	acc := &Account{}
	acc.usageSyncMu.Lock()
	applied := false
	start := time.Now()
	if acc.TryApplyUsageObservation(time.Now(), func() { applied = true }) {
		t.Fatal("TryApply succeeded while the observation lock was held")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("TryApply blocked for %v, want a non-blocking skip", elapsed)
	}
	acc.usageSyncMu.Unlock()
	if applied {
		t.Fatal("skipped observation still ran its callback")
	}

	if !acc.TryApplyUsageObservation(time.Now(), func() { applied = true }) || !applied {
		t.Fatal("TryApply did not run after the lock was released")
	}
}

func TestTryApplyUsageObservationRejectsOlderTimestamp(t *testing.T) {
	acc := &Account{}
	now := time.Now()
	applied := 0
	if !acc.TryApplyUsageObservation(now, func() { applied++ }) {
		t.Fatal("first observation was rejected")
	}
	if acc.TryApplyUsageObservation(now.Add(-time.Minute), func() { applied++ }) {
		t.Fatal("older observation was accepted")
	}
	if applied != 1 {
		t.Fatalf("callbacks = %d, want 1", applied)
	}
}
