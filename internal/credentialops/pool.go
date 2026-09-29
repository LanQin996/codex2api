package credentialops

import (
	"context"
	"sync"
	"time"
)

// RunPool claims serially, executes concurrently, and rereads the limit before
// filling free slots. Lowering the limit drains running tasks without cancelling
// them. Claim must atomically acquire a durable per-account lease.
// The limit is per process, not a cluster-wide concurrency budget.
func RunPool(ctx context.Context, interval time.Duration, limit func(context.Context) (int, error), claim func(context.Context) (func(context.Context), error)) {
	done := make(chan struct{}, 8)
	var wg sync.WaitGroup
	defer wg.Wait()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	active := 0
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := limit(ctx)
		if err == nil && n >= 1 && n <= 8 {
			for active < n && ctx.Err() == nil {
				job, err := claim(ctx)
				if err != nil || job == nil {
					break
				}
				active++
				wg.Add(1)
				go func(run func(context.Context)) {
					defer wg.Done()
					defer func() { done <- struct{}{} }()
					run(ctx)
				}(job)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-done:
			active--
		}
	}
}
