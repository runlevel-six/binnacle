package core

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoalesce_CollapsesBurstIntoOneRebuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int64
	trigger := Coalesce(ctx, 20*time.Millisecond, func() { runs.Add(1) })

	// A burst standing in for a hot object being updated continuously — the shape
	// of a Kubernetes Event that has occurred many thousands of times.
	for range 10_000 {
		trigger()
	}

	// Long enough for a handful of intervals to elapse.
	time.Sleep(120 * time.Millisecond)

	got := runs.Load()
	if got == 0 {
		t.Fatal("a burst should still produce at least one rebuild")
	}
	// Without coalescing this would be 10,000. A few is the whole point.
	if got > 10 {
		t.Errorf("rebuilds: got %d, expected a handful — the burst was not coalesced", got)
	}
}

// A quiet resource must not be rebuilt on a timer. Doing so would re-list and
// re-sort every object twice a second for no reason, and would keep waking the
// dashboard's redraw loop.
func TestCoalesce_IdleDoesNotRebuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int64
	Coalesce(ctx, 10*time.Millisecond, func() { runs.Add(1) })

	time.Sleep(100 * time.Millisecond)
	if got := runs.Load(); got != 0 {
		t.Errorf("idle rebuilds: got %d want 0", got)
	}
}

// Every trigger must eventually be reflected: coalescing may drop intermediate
// states but never the final one, or the display would be permanently stale.
func TestCoalesce_LastChangeAlwaysLands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var latest atomic.Int64
	value := atomic.Int64{}
	trigger := Coalesce(ctx, 10*time.Millisecond, func() { latest.Store(value.Load()) })

	for i := int64(1); i <= 50; i++ {
		value.Store(i)
		trigger()
	}
	time.Sleep(80 * time.Millisecond)

	if got := latest.Load(); got != 50 {
		t.Errorf("final value: got %d want 50", got)
	}
}

// A trigger fired after a rebuild must schedule another one, or a change arriving
// just after a publish would be lost until the next unrelated change.
func TestCoalesce_RearmsAfterRebuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int64
	trigger := Coalesce(ctx, 10*time.Millisecond, func() { runs.Add(1) })

	trigger()
	time.Sleep(40 * time.Millisecond)
	first := runs.Load()

	trigger()
	time.Sleep(40 * time.Millisecond)

	if runs.Load() <= first {
		t.Errorf("a later trigger produced no rebuild: %d then %d", first, runs.Load())
	}
}

// The trigger must never block: informer callbacks share a goroutine, so blocking
// one would stall delivery for every other handler on that informer.
func TestCoalesce_TriggerDoesNotBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A deliberately slow rebuild.
	trigger := Coalesce(ctx, time.Millisecond, func() { time.Sleep(50 * time.Millisecond) })

	done := make(chan struct{})
	go func() {
		for range 1000 {
			trigger()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("trigger blocked behind a slow rebuild")
	}
}

func TestCoalesce_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var runs atomic.Int64
	trigger := Coalesce(ctx, 10*time.Millisecond, func() { runs.Add(1) })

	cancel()
	time.Sleep(30 * time.Millisecond)
	before := runs.Load()

	trigger()
	time.Sleep(50 * time.Millisecond)

	if runs.Load() != before {
		t.Error("rebuilds continued after cancellation")
	}
}
