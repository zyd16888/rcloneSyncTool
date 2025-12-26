package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"115togd/internal/store"
)

type ruleWorker struct {
	st   *store.Store
	rule store.Rule

	pm *PortManager
	gl *GlobalLimiter
	jr *JobRegistry

	sem chan struct{}

	scanCh chan struct{}
	stopCh chan struct{}
	stopped atomic.Bool

	cancelMu sync.Mutex
	cancel   context.CancelFunc
}

func newRuleWorker(st *store.Store, rule store.Rule, pm *PortManager, gl *GlobalLimiter, jr *JobRegistry) *ruleWorker {
	return &ruleWorker{
		st:     st,
		rule:   rule,
		pm:     pm,
		gl:     gl,
		jr:     jr,
		scanCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
		sem:    make(chan struct{}, rule.MaxParallelJobs),
	}
}

func (w *ruleWorker) setCancel(cancel context.CancelFunc) {
	w.cancelMu.Lock()
	w.cancel = cancel
	stopped := w.stopped.Load()
	w.cancelMu.Unlock()
	if stopped && cancel != nil {
		cancel()
	}
}

func (w *ruleWorker) stop() {
	if w.stopped.CompareAndSwap(false, true) {
		close(w.stopCh)
	}
	w.cancelMu.Lock()
	cancel := w.cancel
	w.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *ruleWorker) triggerScan() {
	select {
	case w.scanCh <- struct{}{}:
	default:
	}
}

func (w *ruleWorker) run(ctx context.Context) {
	scanCtx, scanCancel := context.WithCancel(ctx)
	defer scanCancel()
	w.setCancel(scanCancel)

	settings, err := w.st.RuntimeSettings(scanCtx)
	if err != nil {
		log.Printf("rule %s: load settings: %v", w.rule.ID, err)
		return
	}

	scanTicker := time.NewTicker(time.Duration(w.rule.ScanIntervalSec) * time.Second)
	defer scanTicker.Stop()
	schedTicker := time.NewTicker(settings.SchedulerTick)
	defer schedTicker.Stop()

	if w.rule.SrcKind == "local" && w.rule.LocalWatch {
		go w.watchLocal(scanCtx)
	}

	// Prime: run a scan soon.
	w.triggerScan()

	for {
		select {
		case <-scanCtx.Done():
			return
		case <-w.stopCh:
			scanCancel()
			return
		case <-scanTicker.C:
			w.doScan(scanCtx)
		case <-w.scanCh:
			w.doScan(scanCtx)
		case <-schedTicker.C:
			w.doSchedule(scanCtx, ctx)
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
	if _, err := w.st.EnqueueStable(ctx, w.rule.ID, w.rule.BatchSize, w.rule.MinFileSizeBytes); err != nil {
		log.Printf("rule %s: enqueue stable: %v", w.rule.ID, err)
	}
}

func (w *ruleWorker) doSchedule(scanCtx context.Context, jobCtx context.Context) {
	// keep queue warm
	if _, err := w.st.EnqueueStable(scanCtx, w.rule.ID, w.rule.BatchSize, w.rule.MinFileSizeBytes); err != nil {
		log.Printf("rule %s: enqueue stable: %v", w.rule.ID, err)
	}
	for {
		select {
		case <-scanCtx.Done():
			return
		case w.sem <- struct{}{}:
			go w.startOneJob(scanCtx, jobCtx)
			continue
		default:
			return
		}
	}
}

func (w *ruleWorker) startOneJob(scanCtx context.Context, jobCtx context.Context) {
	defer func() { <-w.sem }()

	if w.stopped.Load() || scanCtx.Err() != nil {
		return
	}

	settings, err := w.st.RuntimeSettings(scanCtx)
	if err != nil {
		log.Printf("rule %s: settings: %v", w.rule.ID, err)
		return
	}
	if !w.st.HasQueued(scanCtx, w.rule.ID) {
		return
	}

	limitBytes := w.rule.DailyLimitBytes
	usageFn := func() (int64, error) {
		return w.st.RuleUsageSince(scanCtx, w.rule.ID, time.Now().Add(-24*time.Hour))
	}
	// If grouped, use group logic
	if w.rule.LimitGroup != "" {
		lg, ok, err := w.st.GetLimitGroup(scanCtx, w.rule.LimitGroup)
		if err != nil {
			log.Printf("rule %s: get limit group: %v", w.rule.ID, err)
			return
		}
		if ok {
			limitBytes = lg.DailyLimitBytes
		} else {
			// Group not found? fallback to rule's limit or 0?
			// Let's assume 0 (unlimited) or log warning.
			// Ideally the UI prevents selecting non-existent groups, but user can delete group.
			limitBytes = 0 
		}

		usageFn = func() (int64, error) {
			return w.st.GroupUsageSince(scanCtx, w.rule.LimitGroup, time.Now().Add(-24*time.Hour))
		}
	}

	if limitBytes > 0 {
		usage, err := usageFn()
		if err != nil {
			log.Printf("rule %s: check usage: %v", w.rule.ID, err)
		} else if usage >= limitBytes {
			// Limit reached.
			return
		}
	}

	if w.gl != nil {
		if ok := w.gl.Acquire(scanCtx); !ok {
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
	paths, err := w.st.ClaimQueuedForJob(scanCtx, w.rule, jobID, w.rule.BatchSize)
	if err != nil {
		log.Printf("rule %s: claim queued: %v", w.rule.ID, err)
		return
	}
	if len(paths) == 0 {
		return
	}

	// Pre-check limit with estimated size
	if limitBytes > 0 {
		jobSize, err := w.st.GetJobFilesSize(jobCtx, jobID)
		if err == nil {
			currentUsage, _ := usageFn()
			if currentUsage+jobSize > limitBytes {
				log.Printf("rule %s: daily limit exceeded (usage: %d, job: %d, limit: %d), skipping job %s", 
					w.rule.ID, currentUsage, jobSize, limitBytes, jobID)
				_ = w.st.ReleaseTransferringBackToQueued(jobCtx, jobID)
				return
			}
		} else {
			log.Printf("rule %s: check job size: %v", w.rule.ID, err)
		}
	}

	log.Printf("[Worker] Job %s (Rule: %s) starting with %d files", jobID, w.rule.ID, len(paths))
	if w.stopped.Load() || scanCtx.Err() != nil {
		_ = w.st.ReleaseTransferringBackToQueued(jobCtx, jobID)
		return
	}

	baseDir := filepath.Dir(settings.LogDir)
	jobDir := filepath.Join(baseDir, "jobs", w.rule.ID, jobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		log.Printf("rule %s: mkdir job dir: %v", w.rule.ID, err)
		_ = w.st.ReleaseTransferringBackToQueued(jobCtx, jobID)
		return
	}

	filesFrom := filepath.Join(jobDir, "files.txt")
	if err := os.WriteFile(filesFrom, []byte(strings.Join(paths, "\n")+"\n"), 0o600); err != nil {
		log.Printf("rule %s: write files-from: %v", w.rule.ID, err)
		_ = w.st.ReleaseTransferringBackToQueued(jobCtx, jobID)
		return
	}

	if w.stopped.Load() || scanCtx.Err() != nil {
		_ = w.st.ReleaseTransferringBackToQueued(jobCtx, jobID)
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
	if err := w.st.CreateJobRow(jobCtx, j); err != nil {
		log.Printf("rule %s: create job: %v", w.rule.ID, err)
		_ = w.st.ReleaseTransferringBackToQueued(jobCtx, jobID)
		return
	}

	if w.stopped.Load() || scanCtx.Err() != nil {
		_ = w.st.UpdateJobTerminated(jobCtx, jobID, "rule disabled", 0, 0)
		_ = w.st.ReleaseTransferringBackToQueued(jobCtx, jobID)
		return
	}

	jobCtx, cancel := context.WithCancel(jobCtx)
	defer cancel()

	res := w.runWithMetrics(jobCtx, settings, port, filesFrom, logPath, jobID)
	if res.Err != nil {
		if errors.Is(res.Err, errTerminatedByUser) {
			_ = w.st.UpdateJobTerminated(jobCtx, jobID, "terminated by user", res.BytesDone, res.AvgSpeed)
			doneSet, _ := transferredPathsFromLog(logPath)
			var donePaths []string
			for _, p := range paths {
				if _, ok := doneSet[p]; ok {
					donePaths = append(donePaths, p)
				}
			}
			_ = w.st.FinalizeJobFiles(jobCtx, jobID, donePaths, "queued", "")
			_ = w.st.ClearJobOnDone(jobCtx, jobID)
			return
		}
		if errors.Is(res.Err, errTerminatedBySignal) || errors.Is(res.Err, context.Canceled) {
			_ = w.st.UpdateJobTerminated(jobCtx, jobID, "terminated", res.BytesDone, res.AvgSpeed)
			doneSet, _ := transferredPathsFromLog(logPath)
			var donePaths []string
			for _, p := range paths {
				if _, ok := doneSet[p]; ok {
					donePaths = append(donePaths, p)
				}
			}
			_ = w.st.FinalizeJobFiles(jobCtx, jobID, donePaths, "queued", "")
			_ = w.st.ClearJobOnDone(jobCtx, jobID)
			return
		}
		_ = w.st.UpdateJobFailed(jobCtx, jobID, res.Err.Error(), res.BytesDone, res.AvgSpeed)
		doneSet, _ := transferredPathsFromLog(logPath)
		var donePaths []string
		for _, p := range paths {
			if _, ok := doneSet[p]; ok {
				donePaths = append(donePaths, p)
			}
		}
		_ = w.st.FinalizeJobFiles(jobCtx, jobID, donePaths, "failed", res.Err.Error())
		_ = w.st.ClearJobOnDone(jobCtx, jobID)
		return
	}
	doneSet, err := transferredPathsFromLog(logPath)
	if err != nil {
		_ = w.st.UpdateJobFailed(jobCtx, jobID, "log parse: "+err.Error(), res.BytesDone, res.AvgSpeed)
		_ = w.st.FinalizeJobFiles(jobCtx, jobID, nil, "queued", "")
		return
	}
	var donePaths []string
	for _, p := range paths {
		if _, ok := doneSet[p]; ok {
			donePaths = append(donePaths, p)
		}
	}
	if len(donePaths) != len(paths) {
		// rclone may exit 0 with "There was nothing to transfer" (everything already exists at destination).
		// In that case we should treat all claimed paths as finished to avoid endless re-queue loops.
		if len(donePaths) == 0 && logHadNothingToTransfer(logPath) {
			_ = w.st.UpdateJobDone(jobCtx, jobID, res.BytesDone, res.AvgSpeed)
			_ = w.st.FinalizeJobFiles(jobCtx, jobID, paths, "queued", "")
			_ = w.st.ClearJobOnDone(jobCtx, jobID)
			return
		}
		_ = w.st.UpdateJobFailed(jobCtx, jobID, fmt.Sprintf("incomplete: %d/%d transferred", len(donePaths), len(paths)), res.BytesDone, res.AvgSpeed)
		_ = w.st.FinalizeJobFiles(jobCtx, jobID, donePaths, "queued", "")
		_ = w.st.ClearJobOnDone(jobCtx, jobID)
		return
	}
	_ = w.st.UpdateJobDone(jobCtx, jobID, res.BytesDone, res.AvgSpeed)
	_ = w.st.FinalizeJobFiles(jobCtx, jobID, donePaths, "queued", "")
	_ = w.st.ClearJobOnDone(jobCtx, jobID)
}

func (w *ruleWorker) watchLocal(ctx context.Context) {
	root := strings.TrimSpace(w.rule.SrcLocalRoot)
	if root == "" {
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("rule %s: local watch: %v", w.rule.ID, err)
		return
	}
	defer watcher.Close()

	addDir := func(p string) {
		if err := watcher.Add(p); err != nil {
			// ignore
		}
	}

	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			addDir(p)
		}
		return nil
	})

	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	pending := false
	trigger := func() {
		if pending {
			return
		}
		pending = true
		debounce.Reset(600 * time.Millisecond)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-watcher.Errors:
			if err != nil {
				log.Printf("rule %s: local watch error: %v", w.rule.ID, err)
			}
		case ev := <-watcher.Events:
			// Watch new directories recursively.
			if ev.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				fi, err := os.Stat(ev.Name)
				if err == nil && fi.IsDir() {
					_ = filepath.WalkDir(ev.Name, func(p string, d fs.DirEntry, err error) error {
						if err == nil && d.IsDir() {
							addDir(p)
						}
						return nil
					})
				}
			}
			trigger()
		case <-debounce.C:
			pending = false
			w.triggerScan()
		}
	}
}

type jobResult struct {
	BytesDone int64
	AvgSpeed  float64
	Err       error
}

var errTerminatedByUser = errors.New("terminated by user")
var errTerminatedBySignal = errors.New("terminated by signal")

func (w *ruleWorker) runWithMetrics(ctx context.Context, settings store.RuntimeSettings, port int, filesFromPath, logPath, jobID string) jobResult {
	var src string
	if w.rule.SrcKind == "local" {
		src = w.rule.SrcLocalRoot
	} else {
		src = fmt.Sprintf("%s:%s", w.rule.SrcRemote, w.rule.SrcPath)
	}
	dst := fmt.Sprintf("%s:%s", w.rule.DstRemote, w.rule.DstPath)

	args := []string{
		w.rule.TransferMode,
		src, dst,
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
	if strings.TrimSpace(filesFromPath) != "" {
		args = append(args, "--files-from", filesFromPath)
	}
	if settings.BufferSize != "" {
		args = append(args, "--buffer-size", settings.BufferSize)
	}
	if settings.DriveChunkSize != "" {
		args = append(args, "--drive-chunk-size", settings.DriveChunkSize)
	}
	effectiveBwlimit := strings.TrimSpace(w.rule.Bwlimit)
	if effectiveBwlimit == "" {
		effectiveBwlimit = strings.TrimSpace(settings.Bwlimit)
	}
	if effectiveBwlimit != "" {
		args = append(args, "--bwlimit", effectiveBwlimit)
	}
	if w.rule.MinFileSizeBytes > 0 {
		args = append(args, "--min-size", fmt.Sprintf("%d", w.rule.MinFileSizeBytes))
	}
	if rawExts := strings.ReplaceAll(w.rule.IgnoreExtensions, ",", " "); strings.TrimSpace(rawExts) != "" {
		for _, ext := range strings.Fields(rawExts) {
			if strings.HasPrefix(ext, ".") && !strings.Contains(ext, "*") {
				ext = "*" + ext
			}
			args = append(args, "--exclude", ext)
		}
	}
	if strings.TrimSpace(w.rule.RcloneExtraArgs) != "" {
		parsed, err := ParseRcloneArgs(w.rule.RcloneExtraArgs)
		if err != nil {
			return jobResult{Err: err}
		}
		san := SanitizeRcloneArgs(parsed)
		args = append(args, san.Args...)
	}

	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	log.Printf("[Executor] Job %s: running rclone %s", jobID, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Stdout = nil
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return jobResult{Err: err}
	}
	var h *JobHandle
	if w.jr != nil {
		h = w.jr.Register(jobID, cmd)
		defer w.jr.Unregister(jobID)
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
			res := jobResult{BytesDone: last.Bytes, AvgSpeed: avgSpeed(last.Bytes, start), Err: ctx.Err()}
			if h != nil && h.Terminated() {
				res.Err = errTerminatedByUser
			}
			log.Printf("[Executor] Job %s finished: %v (Done: %d bytes, AvgSpeed: %.2f B/s)", jobID, res.Err, res.BytesDone, res.AvgSpeed)
			return res
		case err := <-done:
			res := jobResult{BytesDone: last.Bytes, AvgSpeed: avgSpeed(last.Bytes, start), Err: err}
			if h != nil && h.Terminated() {
				res.Err = errTerminatedByUser
			}
			if res.Err != nil {
				// keep log in log file; minimal error message here
				var exitErr *exec.ExitError
				if errors.As(res.Err, &exitErr) {
					if st, ok := exitErr.Sys().(syscall.WaitStatus); ok && st.Signaled() {
						res.Err = errTerminatedBySignal
					} else {
						msg := strings.TrimSpace(stderr.String())
						if msg == "" {
							msg = res.Err.Error()
						}
						res.Err = errors.New(msg)
					}
				}
			}
			log.Printf("[Executor] Job %s finished: %v (Done: %d bytes, AvgSpeed: %.2f B/s)", jobID, res.Err, res.BytesDone, res.AvgSpeed)
			return res
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
