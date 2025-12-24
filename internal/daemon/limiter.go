package daemon

import (
	"context"
	"sync/atomic"
	"time"
)

type GlobalLimiter struct {
	limit int64
	sem   chan struct{}
}

func NewGlobalLimiter(limit int) *GlobalLimiter {
	if limit < 0 {
		limit = 0
	}
	return &GlobalLimiter{
		limit: int64(limit),
		sem:   make(chan struct{}, 65535),
	}
}

func (g *GlobalLimiter) SetLimit(limit int) {
	if limit < 0 {
		limit = 0
	}
	if limit > cap(g.sem) {
		limit = cap(g.sem)
	}
	atomic.StoreInt64(&g.limit, int64(limit))
}

func (g *GlobalLimiter) Acquire(ctx context.Context) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		limit := atomic.LoadInt64(&g.limit)
		if limit <= 0 {
			return true
		}
		if int64(len(g.sem)) < limit {
			select {
			case g.sem <- struct{}{}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (g *GlobalLimiter) Release() {
	select {
	case <-g.sem:
	default:
	}
}
