package credentialops

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolConcurrencyHotReloadAndDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var limit atomic.Int32
	limit.Store(2)
	started := make(chan chan struct{}, 16)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		RunPool(ctx, 5*time.Millisecond, func(context.Context) (int, error) { return int(limit.Load()), nil }, func(context.Context) (func(context.Context), error) {
			return func(ctx context.Context) {
				release := make(chan struct{})
				started <- release
				select {
				case <-release:
				case <-ctx.Done():
				}
			}, nil
		})
	}()
	next := func() chan struct{} {
		t.Helper()
		select {
		case r := <-started:
			return r
		case <-time.After(time.Second):
			t.Fatal("no parallel dispatch")
			return nil
		}
	}
	none := func() {
		t.Helper()
		select {
		case <-started:
			t.Fatal("concurrency limit exceeded")
		case <-time.After(40 * time.Millisecond):
		}
	}
	first, second := next(), next()
	none()
	limit.Store(3)
	third := next()
	none()
	limit.Store(1)
	close(first)
	none()
	close(second)
	none()
	close(third)
	next()
	none()
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("pool did not cancel workers")
	}
}

func TestPoolDoesNotClaimWhenSettingsUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	claims := 0
	RunPool(ctx, time.Millisecond, func(context.Context) (int, error) { return 2, errors.New("database unavailable") }, func(context.Context) (func(context.Context), error) { claims++; return nil, nil })
	if claims != 0 {
		t.Fatal("claimed with unknown configuration")
	}
}
