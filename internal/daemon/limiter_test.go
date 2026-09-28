package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConcurrentLimiterNeverOvershoots(t *testing.T) {
	g := NewGlobalLimiter(1)
	var wg sync.WaitGroup
	var running, maxSeen atomic.Int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !g.Acquire(context.Background()) {
				return
			}
			n := running.Add(1)
			for previous := maxSeen.Load(); n > previous; previous = maxSeen.Load() {
				if maxSeen.CompareAndSwap(previous, n) {
					break
				}
			}
			running.Add(-1)
			g.Release()
		}()
	}
	wg.Wait()
	if maxSeen.Load() != 1 {
		t.Fatalf("concurrent limit overshot: %d", maxSeen.Load())
	}
}
func TestLimiterTracksUnlimitedTasksAndRuleReplacement(t *testing.T) {
	g := NewGlobalLimiter(0)
	if !g.TryAcquireRule("rule", 1) {
		t.Fatal("initial acquire failed")
	}
	if g.TryAcquireRule("rule", 1) {
		t.Fatal("rule replacement lost slot")
	}
	g.SetLimit(1)
	if g.TryAcquireRule("other", 1) {
		t.Fatal("unlimited task was not counted after limit change")
	}
	g.ReleaseRule("rule")
	if !g.TryAcquireRule("other", 1) {
		t.Fatal("slot not released")
	}
	g.ReleaseRule("other")
}
