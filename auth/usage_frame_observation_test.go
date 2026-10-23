package auth

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

func waitUsageFrameDrain(t *testing.T, acc *Account) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		acc.usageFrameObservationMu.Lock()
		running := acc.usageFrameObservationRunning
		acc.usageFrameObservationMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("frame observation worker did not drain")
}

func TestUsageFrameObservationPreservesPartialFIFO(t *testing.T) {
	acc := &Account{}
	var store *Store
	at := time.Now()
	var applied []int
	acc.usageSyncMu.Lock()
	var release sync.Once
	unlock := func() { release.Do(acc.usageSyncMu.Unlock) }
	t.Cleanup(unlock)
	for i := 0; i < 3; i++ {
		store.ApplyUsageFrameObservation(acc, at.Add(time.Duration(i)*time.Second), func() {
			applied = append(applied, i)
			switch i {
			case 0:
				acc.SetUsageSnapshot5hAt(42, at.Add(time.Hour), at)
			case 1:
				acc.SetUsageSnapshot(65, at.Add(time.Second))
			case 2:
				acc.ApplySparseCreditsHeaders(true, false, nil, nil, "")
			}
		})
	}
	unlock()
	// A frame arriving as the worker acquires the observation gate must not
	// bypass the backlog and make older partial windows stale.
	store.ApplyUsageFrameObservation(acc, at.Add(3*time.Second), func() { applied = append(applied, 3) })
	waitUsageFrameDrain(t, acc)
	if !reflect.DeepEqual(applied, []int{0, 1, 2, 3}) {
		t.Fatalf("partial observations applied out of order: %v", applied)
	}
	if pct, _, valid := acc.GetUsageSnapshot5h(); !valid || pct != 42 {
		t.Fatalf("5h partial lost: %v/%v", pct, valid)
	}
	if pct, valid := acc.GetUsagePercent7d(); !valid || pct != 65 {
		t.Fatalf("7d partial lost: %v/%v", pct, valid)
	}
	if !acc.CreditsValid || !acc.CreditsHasCredits || !acc.UsageUpdatedAt5h.Equal(at) || !acc.usageObservedAt.Equal(at.Add(3*time.Second)) {
		t.Fatal("credits or original observation timestamp lost")
	}
}

func TestUsageFrameObservationOverflowRequestsProbe(t *testing.T) {
	store := NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	acc := &Account{}
	probed := make(chan struct{}, 2)
	probeRelease := make(chan struct{})
	defer close(probeRelease)
	store.SetUsageProbeFunc(func(ctx context.Context, _ *Account) error {
		probed <- struct{}{}
		select {
		case <-probeRelease:
		case <-ctx.Done():
		}
		return nil
	})
	acc.usageSyncMu.Lock()
	var release sync.Once
	unlock := func() { release.Do(acc.usageSyncMu.Unlock) }
	t.Cleanup(unlock)
	last := -1
	at := time.Now()
	for i := 0; i < 2*maxPendingUsageFrames; i++ {
		store.ApplyUsageFrameObservation(acc, at.Add(time.Duration(i)*time.Millisecond), func() { last = i })
	}
	acc.usageFrameObservationMu.Lock()
	pending := len(acc.usageFramePending)
	acc.usageFrameObservationMu.Unlock()
	if pending > maxPendingUsageFrames {
		t.Fatalf("unbounded frame queue: %d", pending)
	}
	select {
	case <-probed:
	case <-time.After(3 * time.Second):
		t.Fatal("overflow did not trigger authoritative probe")
	}
	unlock()
	waitUsageFrameDrain(t, acc)
	if last != 2*maxPendingUsageFrames-1 {
		t.Fatalf("newest tail lost: %d", last)
	}
	select {
	case <-probed:
		t.Fatal("same overflow burst triggered duplicate probe")
	default:
	}
}

func TestUsageFrameObservationDoesNotApplyAfterStop(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{}
	acc.usageSyncMu.Lock()
	var release sync.Once
	unlock := func() { release.Do(acc.usageSyncMu.Unlock) }
	t.Cleanup(unlock)
	applied := false
	store.ApplyUsageFrameObservation(acc, time.Now(), func() { applied = true })
	store.Stop()
	unlock()
	waitUsageFrameDrain(t, acc)
	if applied {
		t.Fatal("queued observation mutated runtime after Stop")
	}
}
