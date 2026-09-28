package daemon

import (
	"115togd/internal/store"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type TransferSpec struct {
	Operation          string                  `json:"operation"`
	SourceSubpath      string                  `json:"source_subpath"`
	DestinationSubpath string                  `json:"destination_subpath"`
	Files              []store.TransferJobFile `json:"files,omitempty"`
	RuleSnapshot       *store.Rule             `json:"rule,omitempty"`
	Prepared           bool                    `json:"prepared,omitempty"`
	GroupSignature     string                  `json:"group_signature,omitempty"`
}

func (s *Supervisor) StartTransferQueue(ctx context.Context) {
	settings, err := s.st.RuntimeSettings(ctx)
	tick := 2 * time.Second
	if err == nil {
		tick = settings.SchedulerTick
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.drainTransferQueue(ctx)
			if updated, e := s.st.RuntimeSettings(ctx); e == nil && updated.SchedulerTick != tick {
				tick = updated.SchedulerTick
				ticker.Reset(tick)
			}
		}
	}
}
func (s *Supervisor) drainTransferQueue(ctx context.Context) {
	jobs, err := s.st.ClaimableTransferJobs(ctx, 20)
	if err != nil {
		log.Printf("task queue: %v", err)
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
	current, ok, err := s.st.GetRule(ctx, job.RuleID)
	if err != nil {
		return
	}
	if !ok || (job.Origin == store.OriginAPI && !current.APIEnabled) {
		s.failWaitingTask(ctx, job, "规则已删除或不再开放给 API")
		return
	}
	if job.Origin == store.OriginScheduler && !current.Enabled && job.StartedAt.IsZero() {
		_ = s.st.BlockTransferJob(ctx, job.JobID, "rule_paused", "规则已暂停，启用后继续排队")
		return
	}
	spec, err := DecodeStoredTransferSpec(job.RequestJSON)
	if err != nil && job.Origin == store.OriginManual {
		spec = TransferSpec{Operation: current.TransferMode}
		err = nil
	}
	if err != nil {
		s.failWaitingTask(ctx, job, "无法解析持久任务清单："+err.Error())
		return
	}
	if job.Origin == store.OriginAPI && !current.AllowsAPIOperation(spec.Operation) {
		s.failWaitingTask(ctx, job, "规则已不允许该传输操作")
		return
	}
	frozen := current
	if spec.RuleSnapshot != nil {
		frozen = *spec.RuleSnapshot
	}
	if err := frozen.Normalize(); err != nil {
		s.failWaitingTask(ctx, job, err.Error())
		return
	}
	if err := ValidateRcloneArgs(frozen.RcloneExtraArgs); err != nil {
		s.failWaitingTask(ctx, job, err.Error())
		return
	}
	if available, _ := rcloneAvailable(); !available {
		_ = s.st.BlockTransferJob(ctx, job.JobID, store.BlockReasonRcloneUnavailable, "未安装 rclone")
		return
	}
	if !s.globalLimiter.TryAcquireRule(current.ID, current.MaxParallelJobs) {
		_ = s.st.BlockTransferJob(ctx, job.JobID, "concurrency", "等待规则或全局并发名额")
		return
	}
	claimed, err := s.st.ClaimTaskPreparation(ctx, job.JobID)
	if err != nil || !claimed {
		s.globalLimiter.ReleaseRule(current.ID)
		return
	}
	taskCtx, cancel := context.WithCancel(ctx)
	handle := s.jobs.RegisterCancel(job.JobID, cancel)
	s.taskWG.Add(1)
	go func() {
		defer s.taskWG.Done()
		defer cancel()
		defer s.jobs.Unregister(job.JobID)
		defer s.globalLimiter.ReleaseRule(current.ID)
		s.prepareAndRunTask(taskCtx, job, spec, frozen, current, handle)
	}()
}

func (s *Supervisor) prepareAndRunTask(ctx context.Context, job store.TransferJob, spec TransferSpec, frozen, current store.Rule, handle *JobHandle) {
	ctx = withTaskOptions(ctx, frozen)
	if spec.RuleSnapshot == nil {
		spec.RuleSnapshot = &frozen
	}
	fail := func(err error) {
		final, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		status := store.TransferStatusFailed
		if ctx.Err() != nil || handle.Terminated() {
			status = store.TransferStatusTerminated
		}
		_ = s.st.CompleteTask(final, job.JobID, status, job.BytesDone, 0, redactMessage(err.Error()), nil, nil)
	}
	settings, err := s.st.RuntimeSettings(ctx)
	if err != nil {
		fail(err)
		return
	}
	if !spec.Prepared {
		spec, err = prepareTransfer(ctx, frozen, spec, settings)
		if err != nil {
			fail(err)
			return
		}
		encoded, err := EncodeTransferSpec(spec)
		if err != nil {
			fail(err)
			return
		}
		if err := s.st.SavePreparedTask(ctx, job.JobID, encoded, spec.Files, job.GroupKey); err != nil {
			if errors.Is(err, store.ErrGroupBusy) {
				_ = s.st.DeferPreparingTask(ctx, job.JobID, "source_busy", err.Error())
				return
			}
			fail(err)
			return
		}
	}
	bytes, err := taskReservationBytes(ctx, job, spec, settings)
	if err != nil {
		fail(err)
		return
	}
	port, err := s.portManager.Acquire()
	if err != nil {
		_ = s.st.DeferPreparingTask(ctx, job.JobID, "rc_port", "等待空闲 RC 端口")
		return
	}
	defer s.portManager.Release(port)
	if err := s.st.ReserveAndStartTask(ctx, job.JobID, port, current, bytes); err != nil {
		if errors.Is(err, store.ErrQuotaUnavailable) {
			_ = s.st.DeferPreparingTask(ctx, job.JobID, store.BlockReasonQuotaExhausted, "剩余配额不足以启动完整任务，等待配额恢复")
			return
		}
		fail(err)
		return
	}
	s.executeTask(ctx, job, spec, settings, port, handle)
}
func (s *Supervisor) failWaitingTask(ctx context.Context, job store.TransferJob, message string) {
	if err := s.st.CompleteTask(ctx, job.JobID, store.TransferStatusFailed, job.BytesDone, 0, redactMessage(message), nil, nil); err != nil {
		log.Printf("task %s: settle: %v", job.JobID, err)
	}
}
func (s *Supervisor) dispatchTransferCallback(ctx context.Context, jobID string) {
	if err := s.st.SetTransferJobCallbackState(ctx, jobID, "pending"); err != nil {
		return
	}
}
func splitSingleFileSource(p string) (string, string, bool) {
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", false
	}
	return filepath.Dir(p), filepath.Base(p), true
}
func joinRemotePath(root, sub string) string {
	root = strings.TrimSuffix(strings.TrimSpace(root), "/")
	sub = strings.Trim(strings.TrimSpace(sub), "/")
	if sub == "" {
		if root == "" {
			return "/"
		}
		return root
	}
	return root + "/" + sub
}
func joinLocalPath(root, sub string) string {
	if sub == "" {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(sub))
}
func EncodeTransferSpec(spec TransferSpec) (string, error) {
	b, err := json.Marshal(spec)
	return string(b), err
}
func DecodeStoredTransferSpec(raw string) (TransferSpec, error) {
	var spec TransferSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return spec, err
	}
	if spec.Operation != "copy" && spec.Operation != "move" {
		return spec, fmt.Errorf("unsupported operation: %q", spec.Operation)
	}
	for i := range spec.Files {
		if !validRelativeFile(spec.Files[i].Path) {
			return spec, errors.New("持久文件清单包含越界路径")
		}
	}
	return spec, nil
}
func decodeTransferSpec(raw string) (TransferSpec, error) { return DecodeStoredTransferSpec(raw) }
