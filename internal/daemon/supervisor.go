package daemon

import (
	"context"
	"log"
	"sync"
	"time"

	"115togd/internal/store"
)

type Supervisor struct {
	st *store.Store

	mu      sync.Mutex
	workers map[string]*ruleWorker

	globalLimiter *GlobalLimiter
	portManager   *PortManager
}

func NewSupervisor(st *store.Store) *Supervisor {
	return &Supervisor{
		st:            st,
		workers:       map[string]*ruleWorker{},
		globalLimiter: NewGlobalLimiter(0),
		portManager:   NewPortManager(55720, 55800),
	}
}

func (s *Supervisor) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	s.refreshRuntime(ctx)
	s.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			s.stopAll()
			return
		case <-t.C:
			s.refreshRuntime(ctx)
			s.reconcile(ctx)
		}
	}
}

func (s *Supervisor) refreshRuntime(ctx context.Context) {
	rs, err := s.st.RuntimeSettings(ctx)
	if err != nil {
		log.Printf("supervisor: load settings: %v", err)
		return
	}
	s.globalLimiter.SetLimit(rs.GlobalMaxJobs)
	s.portManager.SetRange(rs.RcPortStart, rs.RcPortEnd)
}

func (s *Supervisor) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, w := range s.workers {
		w.stop()
		delete(s.workers, id)
	}
}

func (s *Supervisor) reconcile(ctx context.Context) {
	rules, err := s.st.ListRules(ctx)
	if err != nil {
		log.Printf("supervisor: list rules: %v", err)
		return
	}
	desired := map[string]store.Rule{}
	for _, r := range rules {
		if r.Enabled {
			desired[r.ID] = r
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for id, w := range s.workers {
		r, ok := desired[id]
		if !ok {
			w.stop()
			delete(s.workers, id)
			continue
		}
		if !ruleSame(w.rule, r) {
			w.stop()
			delete(s.workers, id)
		}
	}

	for id, r := range desired {
		if _, ok := s.workers[id]; ok {
			continue
		}
		w := newRuleWorker(s.st, r, s.portManager, s.globalLimiter)
		s.workers[id] = w
		go w.run(ctx)
	}
}

func ruleSame(a, b store.Rule) bool {
	return a.ID == b.ID &&
		a.SrcRemote == b.SrcRemote &&
		a.SrcPath == b.SrcPath &&
		a.DstRemote == b.DstRemote &&
		a.DstPath == b.DstPath &&
		a.TransferMode == b.TransferMode &&
		a.MaxParallelJobs == b.MaxParallelJobs &&
		a.ScanIntervalSec == b.ScanIntervalSec &&
		a.StableSeconds == b.StableSeconds &&
		a.BatchSize == b.BatchSize &&
		a.Enabled == b.Enabled
}

func (s *Supervisor) TriggerScan(ruleID string) bool {
	s.mu.Lock()
	w, ok := s.workers[ruleID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	w.triggerScan()
	return true
}
