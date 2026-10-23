package auth

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestUsageSnapshotWritesAreAccountScoped(t *testing.T) {
	store := &Store{}
	first, second := &Account{}, &Account{}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	go func() {
		_ = store.withUsageSnapshotWrite(first, func() error {
			close(entered)
			<-release
			return nil
		})
		close(done)
	}()
	<-entered
	otherDone := make(chan struct{})
	go func() {
		_ = store.withUsageSnapshotWrite(second, func() error { return nil })
		close(otherDone)
	}()
	select {
	case <-otherDone:
	case <-time.After(time.Second):
		t.Error("a blocked account stalled another account's snapshot writer")
	}
	unblock()
	<-done
	<-otherDone
}

func TestUsageFramePersistReleasesSnapshotLockBeforeCredits(t *testing.T) {
	store, db, acc := newUsageFramePersistenceFixture(t)
	store.ObserveUsageFramePlan(acc, "pro")
	acc.ApplySparseCreditsHeaders(true, false, nil, nil, "")
	acc.creditsPersistMu.Lock()
	var once sync.Once
	unblock := func() { once.Do(acc.creditsPersistMu.Unlock) }
	t.Cleanup(unblock)
	done := make(chan struct{})
	go func() {
		newUsageFramePersistQueue().persist(store, usageFramePersistJob{account: acc, dbID: acc.DBID, observedAt: time.Now()})
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	released := false
	for time.Now().Before(deadline) {
		row, err := db.GetAccountByID(context.Background(), acc.DBID)
		if err == nil && row.GetCredential("plan_type") == "pro" && acc.usageSnapshotPersistMu.TryLock() {
			acc.usageSnapshotPersistMu.Unlock()
			released = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	unblock()
	<-done
	if !released {
		t.Fatal("snapshot lock remained held while credit persistence was blocked")
	}
}

// TestUsageFramePersistSkipsSupersededJob verifies the worker re-checks the
// observation order under the snapshot-write lock: a queued older frame must
// not overwrite a newer WHAM/frame observation that was applied while the job
// waited for the database.
func TestUsageFramePersistSkipsSupersededJob(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "superseded.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAccountWithCredentials(ctx, "superseded", map[string]any{
		"access_token": "at-test", "plan_type": "plus",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	account := &Account{DBID: id, AccessToken: "at-test", PlanType: "plus", Status: StatusReady}
	store.AddAccount(account)

	t0 := time.Now().Add(-time.Minute)
	account.SetUsageSnapshot(10, t0)

	// Block the worker inside the write lock so the newer observation is
	// applied before its superseded check runs.
	account.usageSnapshotPersistMu.Lock()
	store.EnqueueUsageFramePersist(account, t0)
	t1 := t0.Add(30 * time.Second)
	if !account.ApplyUsageObservation(t1, func() { account.SetUsageSnapshot(80, t1) }) {
		t.Fatal("newer observation was rejected")
	}
	account.usageSnapshotPersistMu.Unlock()

	time.Sleep(200 * time.Millisecond)
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = conn.Close() }()
	var raw string
	if err := conn.QueryRow(`SELECT credentials FROM accounts WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if got := gjson.Get(raw, "codex_7d_used_percent").Float(); got == 10 {
		t.Fatalf("superseded job wrote the older snapshot: %v", got)
	}
}

func newUsageFramePersistenceFixture(t *testing.T) (*Store, *database.DB, *Account) {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "frame-effects.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAccountWithCredentials(context.Background(), "synthetic-frame", map[string]any{"plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	tc := cache.NewMemory(1)
	t.Cleanup(func() { _ = tc.Close() })
	store := NewStore(db, tc, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	acc := &Account{DBID: id, PlanType: "plus", Status: StatusReady}
	store.AddAccount(acc)
	return store, db, acc
}

func TestUsageFramePersistSupersededQuotaStillFlushesCurrentEffects(t *testing.T) {
	store, db, acc := newUsageFramePersistenceFixture(t)
	at := time.Now().UTC().Truncate(time.Second)
	reset := at.Add(time.Hour)
	acc.SetUsageSnapshot5hAt(100, reset, at)
	store.MarkUsageFrameCooldowns(acc, at)
	store.ObserveUsageFramePlan(acc, "pro")
	acc.ApplySparseCreditsHeaders(true, false, nil, nil, "")
	// A newer non-frame observation supersedes the quota job, but does not
	// replace its pending credits, plan or cooldown persistence request.
	acc.ApplyUsageObservation(at.Add(time.Second), func() { acc.SetUsageSnapshot5hAt(40, reset, at.Add(time.Second)) })
	queue := newUsageFramePersistQueue()
	queue.persist(store, usageFramePersistJob{account: acc, dbID: acc.DBID, observedAt: at, has5h: true, pct5h: 100, reset5hAt: reset, persistUsage: true})
	row, err := db.GetAccountByID(context.Background(), acc.DBID)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("codex_5h_used_percent") != "" {
		t.Fatal("superseded quota was persisted")
	}
	if row.GetCredential("plan_type") != "pro" || row.GetCredential("codex_credits") == "" {
		t.Fatal("superseded quota job discarded current plan/credits effects")
	}
	if row.CooldownReason != premium5hCooldownReason || !row.CooldownUntil.Valid || !row.CooldownUntil.Time.Equal(reset) {
		t.Fatal("superseded quota job discarded pending cooldown")
	}
}

func TestUsageFramePersist7dOnlyPreservesReset(t *testing.T) {
	store, db, acc := newUsageFramePersistenceFixture(t)
	at := time.Now().UTC().Truncate(time.Second)
	reset := at.Add(7 * 24 * time.Hour)
	queue := newUsageFramePersistQueue()
	queue.persist(store, usageFramePersistJob{account: acc, dbID: acc.DBID, observedAt: at.Add(time.Second), has7d: true, pct7d: 42, reset7dAt: reset, updatedAt7d: at, persistUsage: true})
	row, err := db.GetAccountByID(context.Background(), acc.DBID)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("codex_7d_used_percent") != "42" || row.GetCredential("codex_7d_reset_at") != reset.Format(time.RFC3339) {
		t.Fatal("7d-only frame did not persist its reset")
	}
	if row.GetCredential("codex_5h_used_percent") != "" {
		t.Fatal("7d-only frame fabricated a 5h window")
	}
	if row.GetCredential("codex_usage_updated_at") != at.Format(time.RFC3339) {
		t.Fatal("side-effect observation changed the persisted quota timestamp")
	}
}

func TestUsageFramePersistRejectsReplacedAccount(t *testing.T) {
	store, db, acc := newUsageFramePersistenceFixture(t)
	job := usageFramePersistJob{account: acc, dbID: acc.DBID, observedAt: time.Now(), has5h: true, pct5h: 99, persistUsage: true}
	store.RemoveAccount(acc.DBID)
	store.AddAccount(&Account{DBID: acc.DBID, PlanType: "plus", Status: StatusReady})
	newUsageFramePersistQueue().persist(store, job)
	row, err := db.GetAccountByID(context.Background(), acc.DBID)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("codex_5h_used_percent") != "" {
		t.Fatal("old account job wrote onto its replacement")
	}
}
