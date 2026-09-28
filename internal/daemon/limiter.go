package daemon

import (
	"context"
	"sync"
)

// GlobalLimiter counts tasks even while limits are disabled. Its slots survive
// changes to the configured limit and replacement of individual rule workers.
type GlobalLimiter struct {
	mu      sync.Mutex
	limit   int
	running int
	byRule  map[string]int
	changed chan struct{}
}

func NewGlobalLimiter(limit int) *GlobalLimiter {
	return &GlobalLimiter{limit: max(0, limit), byRule: map[string]int{}, changed: make(chan struct{})}
}
func (g *GlobalLimiter) SetLimit(limit int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if limit = max(0, limit); g.limit != limit {
		g.limit = limit
		g.signal()
	}
}
func (g *GlobalLimiter) signal() { close(g.changed); g.changed = make(chan struct{}) }
func (g *GlobalLimiter) tryAcquire(ruleID string, ruleLimit int) bool {
	if g.limit > 0 && g.running >= g.limit {
		return false
	}
	if ruleLimit > 0 && g.byRule[ruleID] >= ruleLimit {
		return false
	}
	g.running++
	g.byRule[ruleID]++
	return true
}
func (g *GlobalLimiter) Acquire(ctx context.Context) bool { return g.AcquireRule(ctx, "", 0) }
func (g *GlobalLimiter) AcquireRule(ctx context.Context, ruleID string, limit int) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		g.mu.Lock()
		if g.tryAcquire(ruleID, limit) {
			g.mu.Unlock()
			return true
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		}
	}
}
func (g *GlobalLimiter) TryAcquire() bool { return g.TryAcquireRule("", 0) }
func (g *GlobalLimiter) TryAcquireRule(ruleID string, limit int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tryAcquire(ruleID, limit)
}
func (g *GlobalLimiter) Release() { g.ReleaseRule("") }
func (g *GlobalLimiter) ReleaseRule(ruleID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byRule[ruleID] == 0 {
		return
	}
	g.running--
	g.byRule[ruleID]--
	if g.byRule[ruleID] == 0 {
		delete(g.byRule, ruleID)
	}
	g.signal()
}
