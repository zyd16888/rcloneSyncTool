package daemon

import (
	"context"
	"os/exec"
	"sync"
	"sync/atomic"
)

type JobHandle struct {
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	terminated atomic.Bool
}

func (h *JobHandle) Terminated() bool { return h != nil && h.terminated.Load() }

type JobRegistry struct {
	mu sync.Mutex
	m  map[string]*JobHandle
}

func NewJobRegistry() *JobRegistry {
	return &JobRegistry{m: map[string]*JobHandle{}}
}

func (r *JobRegistry) Register(jobID string, cmd *exec.Cmd) *JobHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := &JobHandle{cmd: cmd}
	if existing := r.m[jobID]; existing != nil {
		existing.cmd = cmd
		return existing
	}
	r.m[jobID] = h
	return h
}

func (r *JobRegistry) RegisterCancel(jobID string, cancel context.CancelFunc) *JobHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := &JobHandle{cancel: cancel}
	r.m[jobID] = h
	return h
}

func (r *JobRegistry) Unregister(jobID string) {
	r.mu.Lock()
	delete(r.m, jobID)
	r.mu.Unlock()
}

func (r *JobRegistry) Terminate(jobID string) bool {
	r.mu.Lock()
	h := r.m[jobID]
	defer r.mu.Unlock()
	if h == nil {
		return false
	}
	h.terminated.Store(true)
	if h.cancel != nil {
		h.cancel()
	}
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	return true
}
