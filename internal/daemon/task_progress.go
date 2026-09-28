package daemon

// TaskProgress separates content reused at this execution's start from bytes
// transferred by this execution. A recovered job may already have bytes_done.
type TaskProgress struct {
	PlannedBytes int64
	ReusedBytes  int64
	BaseBytes    int64
	ReadyBytes   int64
	Initialized  bool
}

func (h *JobHandle) ConfigureProgress(planned, reused, baseline int64) {
	h.progressMu.Lock()
	h.progress = TaskProgress{PlannedBytes: planned, ReusedBytes: max(int64(0), min(planned, reused)), BaseBytes: baseline, Initialized: true}
	h.progressMu.Unlock()
}

func (h *JobHandle) Progress(bytes int64, completed bool) TaskProgress {
	if h == nil {
		return TaskProgress{}
	}
	h.progressMu.RLock()
	p := h.progress
	h.progressMu.RUnlock()
	p.ReadyBytes = min(p.PlannedBytes, p.ReusedBytes+max(int64(0), bytes-p.BaseBytes))
	if completed {
		p.ReadyBytes = p.PlannedBytes
	}
	return p
}

func (s *Supervisor) JobProgress(id string, bytes int64, completed bool) TaskProgress {
	s.jobs.mu.Lock()
	h := s.jobs.m[id]
	s.jobs.mu.Unlock()
	return h.Progress(bytes, completed)
}
