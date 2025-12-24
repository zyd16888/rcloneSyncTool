package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"115togd/internal/store"
)

type ruleWorker struct {
	st   *store.Store
	rule store.Rule

	pm *PortManager
	gl *GlobalLimiter

	sem chan struct{}

	scanCh chan struct{}
	stopCh chan struct{}
	stopped atomic.Bool
}

func newRuleWorker(st *store.Store, rule store.Rule, pm *PortManager, gl *GlobalLimiter) *ruleWorker {
	return &ruleWorker{
		st:     st,
		rule:   rule,
		pm:     pm,
		gl:     gl,
		scanCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
		sem:    make(chan struct{}, rule.MaxParallelJobs),
	}
}

func (w *ruleWorker) stop() {
	if w.stopped.CompareAndSwap(false, true) {
		close(w.stopCh)
	}
}

func (w *ruleWorker) triggerScan() {
	select {
	case w.scanCh <- struct{}{}:
	default:
	}
}

func (w *ruleWorker) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	settings, err := w.st.RuntimeSettings(ctx)
	if err != nil {
		log.Printf("rule %s: load settings: %v", w.rule.ID, err)
		return
	}

	scanTicker := time.NewTicker(time.Duration(w.rule.ScanIntervalSec) * time.Second)
	defer scanTicker.Stop()
	schedTicker := time.NewTicker(settings.SchedulerTick)
	defer schedTicker.Stop()

	// Prime: run a scan soon.
	w.triggerScan()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-scanTicker.C:
			w.doScan(ctx)
		case <-w.scanCh:
			w.doScan(ctx)
		case <-schedTicker.C:
			w.doSchedule(ctx)
		}
	}
}

func (w *ruleWorker) doScan(ctx context.Context) {
	settings, err := w.st.RuntimeSettings(ctx)
	if err != nil {
		log.Printf("rule %s: settings: %v", w.rule.ID, err)
		return
	}
	entries, err := scanRule(ctx, w.rule, settings)
	if err != nil {
		log.Printf("rule %s: scan: %v", w.rule.ID, err)
		return
	}
	if err := w.st.UpsertScanEntries(ctx, w.rule, entries); err != nil {
		log.Printf("rule %s: upsert scan: %v", w.rule.ID, err)
		return
	}
	if _, err := w.st.EnqueueStable(ctx, w.rule.ID, w.rule.BatchSize); err != nil {
		log.Printf("rule %s: enqueue stable: %v", w.rule.ID, err)
	}
}

func (w *ruleWorker) doSchedule(ctx context.Context) {
	// keep queue warm
	if _, err := w.st.EnqueueStable(ctx, w.rule.ID, w.rule.BatchSize); err != nil {
		log.Printf("rule %s: enqueue stable: %v", w.rule.ID, err)
	}
	for {
		select {
		case w.sem <- struct{}{}:
			go w.startOneJob(ctx)
			continue
		default:
			return
		}
	}
}

func (w *ruleWorker) startOneJob(ctx context.Context) {
	defer func() { <-w.sem }()

	settings, err := w.st.RuntimeSettings(ctx)
	if err != nil {
		log.Printf("rule %s: settings: %v", w.rule.ID, err)
		return
	}
	if !w.st.HasQueued(ctx, w.rule.ID) {
		return
	}
	if w.gl != nil {
		if ok := w.gl.Acquire(ctx); !ok {
			return
		}
		defer w.gl.Release()
	}
	port, err := w.pm.Acquire()
	if err != nil {
		log.Printf("rule %s: rc port: %v", w.rule.ID, err)
		return
	}
	defer w.pm.Release(port)

	jobID := newID()
	paths, err := w.st.ClaimQueuedForJob(ctx, w.rule, jobID, w.rule.BatchSize)
	if err != nil {
		log.Printf("rule %s: claim queued: %v", w.rule.ID, err)
		return
	}
	if len(paths) == 0 {
		return
	}

	baseDir := filepath.Dir(settings.LogDir)
	jobDir := filepath.Join(baseDir, "jobs", w.rule.ID, jobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		log.Printf("rule %s: mkdir job dir: %v", w.rule.ID, err)
		return
	}

	filesFrom := filepath.Join(jobDir, "files.txt")
	if err := os.WriteFile(filesFrom, []byte(strings.Join(paths, "\n")+"\n"), 0o600); err != nil {
		log.Printf("rule %s: write files-from: %v", w.rule.ID, err)
		return
	}

	logPath := filepath.Join(settings.LogDir, w.rule.ID, jobID+".log")
	j := store.Job{
		JobID:        jobID,
		RuleID:       w.rule.ID,
		TransferMode: w.rule.TransferMode,
		RcPort:       port,
		StartedAt:    time.Now(),
		LogPath:      logPath,
	}
	if err := w.st.CreateJobRow(ctx, j); err != nil {
		log.Printf("rule %s: create job: %v", w.rule.ID, err)
		_ = w.st.ReleaseTransferringBackToQueued(ctx, jobID)
		return
	}

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	res := w.runWithMetrics(jobCtx, settings, port, filesFrom, logPath, jobID)
	if res.Err != nil {
		_ = w.st.UpdateJobFailed(ctx, jobID, res.Err.Error(), res.BytesDone, res.AvgSpeed)
		_ = w.st.MarkJobFiles(ctx, jobID, "failed", res.Err.Error())
		_ = w.st.ReleaseTransferringBackToQueued(ctx, jobID)
		return
	}
	_ = w.st.UpdateJobDone(ctx, jobID, res.BytesDone, res.AvgSpeed)
	_ = w.st.MarkJobFiles(ctx, jobID, "done", "")
	_ = w.st.ClearJobOnDone(ctx, jobID)
}

type jobResult struct {
	BytesDone int64
	AvgSpeed  float64
	Err       error
}

func (w *ruleWorker) runWithMetrics(ctx context.Context, settings store.RuntimeSettings, port int, filesFromPath, logPath, jobID string) jobResult {
	src := fmt.Sprintf("%s:%s", w.rule.SrcRemote, w.rule.SrcPath)
	dst := fmt.Sprintf("%s:%s", w.rule.DstRemote, w.rule.DstPath)

	args := []string{
		w.rule.TransferMode,
		src, dst,
		"--files-from", filesFromPath,
		"--stats", "0",
		"--rc",
		"--rc-no-auth",
		"--rc-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"--log-file", logPath,
		"--log-level", "INFO",
		fmt.Sprintf("--transfers=%d", settings.Transfers),
		fmt.Sprintf("--checkers=%d", settings.Checkers),
	}
	if strings.TrimSpace(settings.RcloneConfigPath) != "" {
		args = append(args, "--config", settings.RcloneConfigPath)
	}
	if settings.BufferSize != "" {
		args = append(args, "--buffer-size", settings.BufferSize)
	}
	if settings.DriveChunkSize != "" {
		args = append(args, "--drive-chunk-size", settings.DriveChunkSize)
	}
	if strings.TrimSpace(settings.Bwlimit) != "" {
		args = append(args, "--bwlimit", settings.Bwlimit)
	}

	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Stdout = nil
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return jobResult{Err: err}
	}

	start := time.Now()
	readyUntil := time.Now().Add(10 * time.Second)
	var last rcStats
	for time.Now().Before(readyUntil) {
		s, err := pollRC(ctx, port)
		if err == nil {
			last = s
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ticker := time.NewTicker(settings.MetricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = <-done
			return jobResult{BytesDone: last.Bytes, AvgSpeed: avgSpeed(last.Bytes, start), Err: ctx.Err()}
		case err := <-done:
			if err != nil {
				// keep log in log file; minimal error message here
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					msg := strings.TrimSpace(stderr.String())
					if msg == "" {
						msg = err.Error()
					}
					return jobResult{BytesDone: last.Bytes, AvgSpeed: avgSpeed(last.Bytes, start), Err: errors.New(msg)}
				}
				msg := strings.TrimSpace(stderr.String())
				if msg == "" {
					msg = err.Error()
				}
				return jobResult{BytesDone: last.Bytes, AvgSpeed: avgSpeed(last.Bytes, start), Err: errors.New(msg)}
			}
			return jobResult{BytesDone: last.Bytes, AvgSpeed: avgSpeed(last.Bytes, start), Err: nil}
		case <-ticker.C:
			s, err := pollRC(ctx, port)
			if err != nil {
				continue
			}
			last = s
			_ = w.st.InsertJobMetric(ctx, store.JobMetric{
				JobID:     jobID,
				Ts:        time.Now(),
				Bytes:     s.Bytes,
				Speed:     s.Speed,
				Transfers: s.Transfers,
				Errors:    s.Errors,
			})
			_ = w.st.UpdateJobRunningStats(ctx, jobID, s.Bytes, s.Speed)
		}
	}
}
