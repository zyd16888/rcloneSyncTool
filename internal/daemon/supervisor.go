package daemon

import (
	"context"
	"errors"
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
	jobs          *JobRegistry

	rootCtx context.Context
}

func NewSupervisor(st *store.Store) *Supervisor {
	return &Supervisor{
		st:            st,
		workers:       map[string]*ruleWorker{},
		globalLimiter: NewGlobalLimiter(0),
		portManager:   NewPortManager(55720, 55800),
		jobs:          NewJobRegistry(),
	}
}

func (s *Supervisor) Run(ctx context.Context) {
	s.rootCtx = ctx
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
		w := newRuleWorker(s.st, r, s.portManager, s.globalLimiter, s.jobs)
		s.workers[id] = w
		go w.run(ctx)
	}
}

func ruleSame(a, b store.Rule) bool {
	return a.ID == b.ID &&
		a.SrcKind == b.SrcKind &&
		a.SrcRemote == b.SrcRemote &&
		a.SrcPath == b.SrcPath &&
		a.SrcLocalRoot == b.SrcLocalRoot &&
		a.LocalWatch == b.LocalWatch &&
		a.DstRemote == b.DstRemote &&
		a.DstPath == b.DstPath &&
		a.TransferMode == b.TransferMode &&
		a.Bwlimit == b.Bwlimit &&
		a.MinFileSizeBytes == b.MinFileSizeBytes &&
		a.IsManual == b.IsManual &&
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

func (s *Supervisor) StopRule(ruleID string) bool {
	s.mu.Lock()
	w, ok := s.workers[ruleID]
	if ok {
		delete(s.workers, ruleID)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	w.stop()
	return true
}

func (s *Supervisor) TerminateJob(jobID string) bool {
	if s.jobs == nil {
		return false
	}
	return s.jobs.Terminate(jobID)
}

func (s *Supervisor) StartManualJob(rule store.Rule, jobID string, logPath string) {
	ctx := s.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go s.runManualJob(ctx, rule, jobID, logPath)
}

func (s *Supervisor) runManualJob(ctx context.Context, rule store.Rule, jobID string, logPath string) {
	settings, err := s.st.RuntimeSettings(ctx)
	if err != nil {
		_ = s.st.UpdateJobFailed(ctx, jobID, "load settings: "+err.Error(), 0, 0)
		return
	}
	if s.globalLimiter != nil {
		if ok := s.globalLimiter.Acquire(ctx); !ok {
			_ = s.st.UpdateJobFailed(ctx, jobID, "acquire global limiter failed", 0, 0)
			return
		}
		defer s.globalLimiter.Release()
	}
	port, err := s.portManager.Acquire()
	if err != nil {
		_ = s.st.UpdateJobFailed(ctx, jobID, "acquire rc port: "+err.Error(), 0, 0)
		return
	}
	defer s.portManager.Release(port)

	_ = s.st.UpdateJobRunning(ctx, jobID, port)

	w := &ruleWorker{st: s.st, rule: rule, jr: s.jobs}
	res := w.runWithMetrics(ctx, settings, port, "", logPath, jobID)
	if res.Err != nil {
		if errors.Is(res.Err, errTerminatedByUser) {
			_ = s.st.UpdateJobTerminated(ctx, jobID, "terminated by user", res.BytesDone, res.AvgSpeed)
			return
		}
		_ = s.st.UpdateJobFailed(ctx, jobID, res.Err.Error(), res.BytesDone, res.AvgSpeed)
		return
	}
	_ = s.st.UpdateJobDone(ctx, jobID, res.BytesDone, res.AvgSpeed)
}
