package daemon

import (
	"115togd/internal/store"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

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
		"--stats", "1s",
		"--use-json-log",
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
		// Newer rclone versions forbid combining --files-from with any other filter options (e.g. --exclude).
		// Use --files-from-raw so extension filters and user extra args keep working together.
		args = append(args, "--files-from-raw", filesFromPath)
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
	if w.rule.ResumeEnabled && strings.TrimSpace(w.rule.PartialSuffix) != "" {
		args = append(args, "--partial-suffix", w.rule.PartialSuffix)
	}
	if w.rule.MinFileSizeBytes > 0 {
		// When using --files-from/--files-from-raw, rclone forbids combining with any other filter options.
		// min_file_size is already enforced by our scan/enqueue/claim logic for automatic jobs.
		if strings.TrimSpace(filesFromPath) == "" {
			args = append(args, "--min-size", fmt.Sprintf("%d", w.rule.MinFileSizeBytes))
		}
	}
	// ignore_extensions is enforced in scan/DB for automatic jobs, so we only pass excludes for manual runs.
	if strings.TrimSpace(filesFromPath) == "" {
		if rawExts := strings.ReplaceAll(w.rule.IgnoreExtensions, ",", " "); strings.TrimSpace(rawExts) != "" {
			for _, ext := range strings.Fields(rawExts) {
				if strings.HasPrefix(ext, ".") && !strings.Contains(ext, "*") {
					ext = "*" + ext
				}
				args = append(args, "--exclude", ext)
			}
		}
	}
	if strings.TrimSpace(w.rule.RcloneExtraArgs) != "" {
		parsed, err := ParseRcloneArgs(w.rule.RcloneExtraArgs)
		if err != nil {
			return jobResult{Err: err}
		}
		san := SanitizeRcloneArgs(parsed)
		if strings.TrimSpace(filesFromPath) != "" {
			san = SanitizeRcloneFilterArgs(san.Args)
		}
		args = append(args, san.Args...)
	}

	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	log.Printf("[Executor] Job %s: running rclone %s", jobID, redactMessage(strings.Join(args, " ")))
	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Stdout = nil
	var stderr strings.Builder
	cmd.Stderr = &stderr

	logOffset := int64(0)
	if info, e := os.Stat(logPath); e == nil {
		logOffset = info.Size()
	}
	if err := cmd.Start(); err != nil {
		return jobResult{Err: err}
	}
	var h *JobHandle
	if w.jr != nil {
		h = w.jr.Register(jobID, cmd)

	}

	baseBytes := int64(0)
	if previous, ok, e := w.st.GetJob(ctx, jobID); e == nil && ok {
		baseBytes = previous.BytesDone
	}
	start := time.Now()
	var last rcStats
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
			if final, ok := statsFromLog(logPath, logOffset); ok {
				last = final
			}
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
				Bytes:     baseBytes + s.Bytes,
				Speed:     s.Speed,
				Transfers: s.Transfers,
				Errors:    s.Errors,
			})
		}
	}
}
