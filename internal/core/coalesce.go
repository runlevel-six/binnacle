// Package core holds helpers shared by the data sources.
package core

import (
	"context"
	"sync"
	"time"
)

// PublishInterval is the shortest gap between two publishes of the same key.
//
// Half a second is imperceptible on a monitoring dashboard but bounds the work
// per key to twice a second however fast the underlying resource churns.
const PublishInterval = 500 * time.Millisecond

// Coalesce returns a trigger function that runs rebuild at most once per
// interval, and only when something has actually changed since the last run.
//
// This exists because informer callbacks fire per object change, and rebuilding a
// snapshot means re-listing, re-projecting and re-sorting *every* object of that
// kind. On a quiet resource that is free. On a busy one it is quadratic in
// disguise: a single Kubernetes Event that has occurred 150,000 times is one
// object being updated continuously, and each update was triggering a full
// rebuild of every event in the cluster plus a re-render of the dashboard. The
// result was a screen that took a long time to appear and then stuttered.
//
// Coalescing is the right shape rather than filtering the noisy object out,
// because the problem is the *rate* of rebuilds, not which object caused them —
// any resource can become the busy one.
//
// The returned trigger is safe for concurrent use and never blocks: informer
// callbacks run on the shared processor's goroutine, so blocking one would stall
// event delivery for every other handler on that informer.
func Coalesce(ctx context.Context, interval time.Duration, rebuild func()) func() {
	var mu sync.Mutex
	dirty := false

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				mu.Lock()
				changed := dirty
				dirty = false
				mu.Unlock()
				if changed {
					rebuild()
				}
			}
		}
	}()

	return func() {
		mu.Lock()
		dirty = true
		mu.Unlock()
	}
}
