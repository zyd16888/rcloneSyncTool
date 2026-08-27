package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"115togd/internal/store"
)

// TransferSpec is one accepted API transfer request, already validated by the
// server layer. Everything here is relative to the rule roots; the daemon adds
// no caller-supplied rclone arguments of its own.
type TransferSpec struct {
	Operation          string
	SourceSubpath      string
	DestinationSubpath string
	Files              []store.TransferJobFile
}

// StartTransferQueue drains API transfer jobs. Running this as a loop rather
// than a goroutine per request is what lets a job wait for quota, survive a
// restart, and resume without the client resubmitting.
func (s *Supervisor) StartTransferQueue(ctx context.Context) {
	settings, err := s.st.RuntimeSettings(ctx)
	tick := 2 * time.Second
	if err == nil && settings.SchedulerTick > 0 {
		tick = settings.SchedulerTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.drainTransferQueue(ctx)
		}
	}
}

func (s *Supervisor) drainTransferQueue(ctx context.Context) {
	jobs, err := s.st.ClaimableTransferJobs(ctx, 20)
	if err != nil {
		log.Printf("transfer queue: list jobs: %v", err)
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		s.tryStartTransferJob(ctx, job)
	}
}

func (s *Supervisor) tryStartTransferJob(ctx context.Context, job store.TransferJob) {
	rule, ok, err := s.st.GetRule(ctx, job.RuleID)
	if err != nil {
		log.Printf("transfer job %s: load rule: %v", job.JobID, err)
		return
	}
	if !ok || !rule.APIEnabled {
		// The rule was deleted or closed to the API after the job was accepted.
		// Failing is honest: the transfer cannot be performed as requested.
		_ = s.st.FinishTransferJob(ctx, job.JobID, store.TransferStatusFailed, 0, 0,
			"rule is no longer available to the API", nil)
		return
	}
	if available, _ := rcloneAvailable(); !available {
		_ = s.st.BlockTransferJob(ctx, job.JobID, store.BlockReasonRcloneUnavailable,
			"rclone is not installed on the transfer host")
		return
	}
	exceeded, err := s.quotaExceeded(ctx, rule)
	if err != nil {
		log.Printf("transfer job %s: quota check: %v", job.JobID, err)
		return
	}
	if exceeded {
		_ = s.st.BlockTransferJob(ctx, job.JobID, store.BlockReasonQuotaExhausted,
			"daily transfer quota reached, job will resume when the window rolls")
		return
	}

	spec, err := decodeTransferSpec(job.RequestJSON)
	if err != nil {
		_ = s.st.FinishTransferJob(ctx, job.JobID, store.TransferStatusFailed, 0, 0,
			"invalid stored request: "+err.Error(), nil)
		return
	}

	if s.globalLimiter != nil && !s.globalLimiter.TryAcquire() {
		return // Capacity is busy; the next tick retries without changing state.
	}
	port, err := s.portManager.Acquire()
	if err != nil {
		if s.globalLimiter != nil {
			s.globalLimiter.Release()
		}
		return
	}
	started, err := s.st.StartTransferJob(ctx, job.JobID, port)
	if err != nil || !started {
		s.portManager.Release(port)
		if s.globalLimiter != nil {
			s.globalLimiter.Release()
		}
		return
	}
	go func() {
		defer s.portManager.Release(port)
		if s.globalLimiter != nil {
			defer s.globalLimiter.Release()
		}
		s.runTransferJob(ctx, rule, job, spec, port)
	}()
}

// quotaExceeded mirrors the scheduler budget rule so an API job never bypasses
// a limit group that the Web UI enforces.
func (s *Supervisor) quotaExceeded(ctx context.Context, rule store.Rule) (bool, error) {
	limit := rule.DailyLimitBytes
	since := time.Now().Add(-24 * time.Hour)
	if strings.TrimSpace(rule.LimitGroup) != "" {
		group, ok, err := s.st.GetLimitGroup(ctx, rule.LimitGroup)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		limit = group.DailyLimitBytes
		if limit <= 0 {
			return false, nil
		}
		used, err := s.st.GroupBudgetSince(ctx, rule.LimitGroup, since)
		if err != nil {
			return false, err
		}
		return used >= limit, nil
	}
	if limit <= 0 {
		return false, nil
	}
	used, err := s.st.RuleBudgetSince(ctx, rule.ID, since)
	if err != nil {
		return false, err
	}
	return used >= limit, nil
}

func (s *Supervisor) runTransferJob(
	ctx context.Context,
	rule store.Rule,
	job store.TransferJob,
	spec TransferSpec,
	port int,
) {
	settings, err := s.st.RuntimeSettings(ctx)
	if err != nil {
		_ = s.st.FinishTransferJob(ctx, job.JobID, store.TransferStatusFailed, 0, 0,
			"load settings: "+err.Error(), nil)
		return
	}

	// The effective rule keeps the identity, quota group, bandwidth limit and
	// extra args of the real rule, and only narrows the source and destination
	// to the validated subpaths.
	effective := rule
	effective.TransferMode = spec.Operation
	if rule.SrcKind == "local" {
		effective.SrcLocalRoot = joinLocalPath(rule.SrcLocalRoot, spec.SourceSubpath)
	} else {
		effective.SrcPath = joinRemotePath(rule.SrcPath, spec.SourceSubpath)
	}
	effective.DstPath = joinRemotePath(rule.DstPath, spec.DestinationSubpath)

	baseDir := filepath.Dir(settings.LogDir)
	jobDir := filepath.Join(baseDir, "jobs", rule.ID, job.JobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		_ = s.st.FinishTransferJob(ctx, job.JobID, store.TransferStatusFailed, 0, 0,
			"mkdir job dir: "+err.Error(), nil)
		return
	}
	if err := os.MkdirAll(filepath.Dir(job.LogPath), 0o755); err != nil {
		_ = s.st.FinishTransferJob(ctx, job.JobID, store.TransferStatusFailed, 0, 0,
			"mkdir log dir: "+err.Error(), nil)
		return
	}

	// A single-file source needs a directory pair plus an explicit file list:
	// `rclone move file dst` treats dst as the new file name, which would
	// silently rename the media. Only the host that owns the filesystem can
	// tell a file from a directory, so the decision is made here rather than
	// guessed by the caller.
	if len(spec.Files) == 0 && rule.SrcKind == "local" {
		if parent, base, ok := splitSingleFileSource(effective.SrcLocalRoot); ok {
			effective.SrcLocalRoot = parent
			spec.Files = []store.TransferJobFile{{Path: base, State: "pending"}}
			_ = s.st.ReplaceTransferJobFiles(ctx, job.JobID, spec.Files)
		}
	}

	filesFrom := ""
	if len(spec.Files) > 0 {
		filesFrom = filepath.Join(jobDir, "files.txt")
		var b strings.Builder
		for _, f := range spec.Files {
			b.WriteString(f.Path)
			b.WriteString("\n")
		}
		if err := os.WriteFile(filesFrom, []byte(b.String()), 0o600); err != nil {
			_ = s.st.FinishTransferJob(ctx, job.JobID, store.TransferStatusFailed, 0, 0,
				"write files-from: "+err.Error(), nil)
			return
		}
	}

	worker := &ruleWorker{st: s.st, rule: effective, jr: s.jobs}
	res := worker.runWithMetrics(ctx, settings, port, filesFrom, job.LogPath, job.JobID)

	done, _ := transferredPathsFromLog(job.LogPath)
	donePaths := make([]string, 0, len(done))
	for path := range done {
		donePaths = append(donePaths, path)
	}

	status := store.TransferStatusDone
	errMsg := ""
	remaining := "failed"
	switch {
	case res.Err == nil:
		remaining = "done"
	case errors.Is(res.Err, errTerminatedByUser),
		errors.Is(res.Err, errTerminatedBySignal),
		errors.Is(res.Err, context.Canceled):
		status = store.TransferStatusTerminated
		errMsg = "terminated"
	default:
		status = store.TransferStatusFailed
		errMsg = res.Err.Error()
	}

	if len(spec.Files) > 0 {
		_ = s.st.MarkTransferJobFileStates(ctx, job.JobID, donePaths, remaining, errMsg)
	} else {
		// Without a declared file list, the transferred set from the log is the
		// only record of what actually moved.
		files := make([]store.TransferJobFile, 0, len(donePaths))
		for _, path := range donePaths {
			files = append(files, store.TransferJobFile{Path: path, State: "done"})
		}
		_ = s.st.ReplaceTransferJobFiles(ctx, job.JobID, files)
	}

	counts, _ := s.st.TransferJobFileCounts(ctx, job.JobID)
	if status == store.TransferStatusDone && counts.Failed > 0 {
		errMsg = fmt.Sprintf("%d file(s) failed to transfer", counts.Failed)
	}
	manifest := map[string]any{
		"operation": spec.Operation,
		"rule_id":   rule.ID,
		"destination": map[string]any{
			"remote":  rule.DstRemote,
			"root":    rule.DstPath,
			"subpath": spec.DestinationSubpath,
			"path":    fmt.Sprintf("%s:%s", rule.DstRemote, effective.DstPath),
		},
		"totals": map[string]any{
			"files_total":  counts.Total,
			"files_done":   counts.Done,
			"files_failed": counts.Failed,
			"bytes_done":   res.BytesDone,
		},
	}
	_ = s.st.FinishTransferJob(ctx, job.JobID, status, res.BytesDone, res.AvgSpeed, errMsg, manifest)

	if strings.TrimSpace(job.CallbackURL) != "" {
		s.dispatchTransferCallback(ctx, job.JobID)
	}
}

// dispatchTransferCallback queues one delivery attempt. The queue owns retries
// so a slow or unreachable receiver never blocks the transfer worker.
func (s *Supervisor) dispatchTransferCallback(ctx context.Context, jobID string) {
	if err := s.st.SetTransferJobCallbackState(ctx, jobID, "pending"); err != nil {
		return
	}
	job, ok, err := s.st.GetTransferJob(ctx, jobID)
	if err != nil || !ok {
		return
	}
	go s.deliverCallback(ctx, job)
}

// splitSingleFileSource reports the parent directory and name of a source that
// resolves to a regular file. A missing or non-regular path keeps directory
// semantics so a transient stat failure never rewrites the transfer.
func splitSingleFileSource(path string) (string, string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return "", "", false
	}
	parent := filepath.Dir(path)
	base := filepath.Base(path)
	if parent == "" || base == "" || base == "." {
		return "", "", false
	}
	return parent, base, true
}

func joinRemotePath(root, sub string) string {
	root = strings.TrimSuffix(strings.TrimSpace(root), "/")
	sub = strings.Trim(strings.TrimSpace(sub), "/")
	if sub == "" {
		return root
	}
	return root + "/" + sub
}

func joinLocalPath(root, sub string) string {
	sub = strings.Trim(strings.TrimSpace(sub), "/")
	if sub == "" {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(sub))
}

// storedTransferSpec is the persisted shape of an accepted request. It is the
// contract between the server layer that validates a submission and the daemon
// that later executes it, including after a restart.
type storedTransferSpec struct {
	Operation          string `json:"operation"`
	SourceSubpath      string `json:"source_subpath"`
	DestinationSubpath string `json:"destination_subpath"`
	Files              []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	} `json:"files"`
}

// EncodeTransferSpec renders a validated request for persistence.
func EncodeTransferSpec(spec TransferSpec) (string, error) {
	stored := storedTransferSpec{
		Operation:          spec.Operation,
		SourceSubpath:      spec.SourceSubpath,
		DestinationSubpath: spec.DestinationSubpath,
	}
	for _, f := range spec.Files {
		stored.Files = append(stored.Files, struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		}{Path: f.Path, Size: f.Size})
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeStoredTransferSpec re-reads a persisted request so a retry can reuse
// the exact spec that was originally validated.
func DecodeStoredTransferSpec(raw string) (TransferSpec, error) {
	return decodeTransferSpec(raw)
}

func decodeTransferSpec(raw string) (TransferSpec, error) {
	var stored storedTransferSpec
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return TransferSpec{}, err
	}
	spec := TransferSpec{
		Operation:          stored.Operation,
		SourceSubpath:      stored.SourceSubpath,
		DestinationSubpath: stored.DestinationSubpath,
	}
	for _, f := range stored.Files {
		spec.Files = append(spec.Files, store.TransferJobFile{Path: f.Path, Size: f.Size})
	}
	if spec.Operation != "copy" && spec.Operation != "move" {
		return TransferSpec{}, fmt.Errorf("unsupported operation: %q", spec.Operation)
	}
	return spec, nil
}

func rcloneAvailable() (bool, string) {
	path, err := exec.LookPath("rclone")
	if err != nil {
		return false, ""
	}
	return true, path
}
