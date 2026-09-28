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
	"time"

	"115togd/internal/store"
)

func rcloneAvailable() (bool, string) { p, err := exec.LookPath("rclone"); return err == nil, p }

func (s *Supervisor) executeTask(ctx context.Context, job store.TransferJob, spec TransferSpec, settings store.RuntimeSettings, port int, handle *JobHandle) {
	started := time.Now()
	bytes := job.BytesDone
	var donePaths []string
	err := s.performTask(ctx, &job, spec, settings, port, &bytes, &donePaths)
	status := store.TransferStatusDone
	message := ""
	if err != nil {
		status = store.TransferStatusFailed
		message = redactMessage(err.Error())
	}
	if handle.Terminated() || errors.Is(err, context.Canceled) || errors.Is(err, errTerminatedByUser) {
		status = store.TransferStatusTerminated
		message = "任务已终止，可从文件清单重试"
	}
	final, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rule := *spec.RuleSnapshot
	result := map[string]any{
		"operation": spec.Operation,
		"rule_id":   rule.ID,
		"phase":     job.Phase,
		"destination": map[string]any{
			"remote":  rule.DstRemote,
			"root":    rule.DstPath,
			"subpath": spec.DestinationSubpath,
			"path":    destinationSpec(effectiveRule(rule, spec)),
		},
		"totals": map[string]any{
			"files_total":  len(spec.Files),
			"files_done":   len(donePaths),
			"files_failed": len(spec.Files) - len(donePaths),
			"bytes_done":   bytes,
		},
	}
	speed := float64(max(int64(0), bytes-job.BytesDone)) / max(1.0, time.Since(started).Seconds())
	if e := s.st.CompleteTask(final, job.JobID, status, bytes, speed, message, donePaths, result); e != nil {
		log.Printf("task %s: save result: %v", job.JobID, e)
	}
	if job.CallbackURL != "" {
		s.dispatchTransferCallback(final, job.JobID)
	}
}

func (s *Supervisor) performTask(ctx context.Context, job *store.TransferJob, spec TransferSpec, settings store.RuntimeSettings, port int, bytes *int64, donePaths *[]string) error {
	rule := effectiveRule(*spec.RuleSnapshot, spec)
	jobDir := filepath.Join(filepath.Dir(settings.LogDir), "jobs", job.RuleID, job.JobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return err
	}
	if !rule.AtomicPublish {
		if _, err := sourceUnchanged(ctx, rule, settings, spec.Files, false); err != nil {
			return err
		}
		list, err := writeTaskFileList(jobDir, spec.Files)
		if err != nil {
			return err
		}
		worker := &ruleWorker{st: s.st, rule: rule, jr: s.jobs}
		res := worker.runWithMetrics(ctx, settings, port, list, job.LogPath, job.JobID)
		*bytes += res.BytesDone
		entries, verifyErr := listDestination(ctx, rule, settings)
		if verifyErr == nil {
			*donePaths = verifiedPaths(spec.Files, entries)
		}
		if spec.Operation == "move" {
			present, sourceErr := sourceUnchanged(ctx, rule, settings, spec.Files, true)
			if sourceErr != nil {
				return sourceErr
			}
			pending := map[string]bool{}
			for _, p := range present {
				pending[p] = true
			}
			var moved []string
			for _, p := range *donePaths {
				if !pending[p] {
					moved = append(moved, p)
				}
			}
			*donePaths = moved
		} else if _, sourceErr := sourceUnchanged(ctx, rule, settings, spec.Files, false); sourceErr != nil {
			*donePaths = nil
			return sourceErr
		}
		if res.Err != nil {
			return res.Err
		}
		if verifyErr != nil {
			return verifyErr
		}
		if len(*donePaths) != len(spec.Files) {
			return fmt.Errorf("任务尚未完成：已验证 %d/%d 个文件", len(*donePaths), len(spec.Files))
		}
		job.Phase = "completed"
		return s.st.SetTaskPhase(ctx, job.JobID, job.Phase, "")
	}
	if err := ensureDestinationParent(ctx, rule, settings); err != nil {
		return err
	}
	exists, err := destinationExists(ctx, rule, settings)
	if err != nil {
		return err
	}
	if exists {
		if job.Phase != "publishing" && job.Phase != "cleanup" {
			return errors.New("正式影片目录已存在，不能将其他任务的文件作为本次发布结果；源文件和暂存内容已保留")
		}
		if job.Phase == "publishing" {
			if job.StagePath == "" {
				return errors.New("缺少原暂存目录，无法确认发布是否完成")
			}
			stageRule := rule
			stageRule.DstPath = job.StagePath
			stageExists, err := destinationExists(ctx, stageRule, settings)
			if err != nil {
				return err
			}
			if stageExists {
				return errors.New("正式目录和原暂存目录同时存在，发布存在冲突；源文件和暂存内容已保留")
			}
		}
		entries, err := listDestination(ctx, rule, settings)
		if err != nil {
			return err
		}
		if !manifestMatches(spec.Files, entries) {
			return errors.New("正式影片目录已存在但与完整清单不符；暂存内容已保留，请先处理目标目录冲突")
		}
		job.Phase = "cleanup"
	} else {
		if job.Phase == "cleanup" {
			return errors.New("已发布的目标目录丢失，保留源文件并停止清理")
		}
		if _, err := sourceUnchanged(ctx, rule, settings, spec.Files, false); err != nil {
			return err
		}
		if err := assertCompleteSource(ctx, rule, settings, spec.Files); err != nil {
			return err
		}
		if job.StagePath == "" {
			job.StagePath = joinRemotePath(rule.StagingPath, job.JobID)
		}
		if err := validateStage(rule, job.StagePath); err != nil {
			return err
		}
		stageRule := rule
		stageRule.DstPath = job.StagePath
		stageRule.TransferMode = "copy"
		job.Phase = "copying"
		if err := s.st.SetTaskPhase(ctx, job.JobID, job.Phase, job.StagePath); err != nil {
			return err
		}
		remaining, err := stagedFilesToTransfer(ctx, rule, settings, job.StagePath, spec.Files)
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			list, err := writeTaskFileList(jobDir, remaining)
			if err != nil {
				return err
			}
			worker := &ruleWorker{st: s.st, rule: stageRule, jr: s.jobs}
			res := worker.runWithMetrics(ctx, settings, port, list, job.LogPath, job.JobID)
			*bytes += res.BytesDone
			if res.Err != nil {
				return res.Err
			}
		}
		job.Phase = "verifying"
		if err := s.st.SetTaskPhase(ctx, job.JobID, job.Phase, job.StagePath); err != nil {
			return err
		}
		staged, err := listDestination(ctx, stageRule, settings)
		if err != nil {
			return err
		}
		if !manifestMatches(spec.Files, staged) || len(staged) != len(spec.Files) {
			return errors.New("暂存目录未通过整组文件数量/大小校验")
		}
		if err := assertCompleteSource(ctx, rule, settings, spec.Files); err != nil {
			return err
		}
		job.Phase = "publishing"
		if err := s.st.SetTaskPhase(ctx, job.JobID, job.Phase, job.StagePath); err != nil {
			return err
		}
		// Recheck immediately before the directory metadata operation.
		if occupied, err := destinationExists(ctx, rule, settings); err != nil {
			return err
		} else if occupied {
			return errors.New("发布前正式目录已被占用，暂存内容已保留")
		}
		if err := publishDirectory(ctx, rule, settings, job.StagePath); err != nil {
			return err
		}
		published, err := listDestination(ctx, rule, settings)
		if err != nil {
			return err
		}
		if !manifestMatches(spec.Files, published) {
			return errors.New("发布后的影片目录校验失败，保留源文件")
		}
		job.Phase = "cleanup"
	}
	if err := s.st.SetTaskPhase(ctx, job.JobID, job.Phase, job.StagePath); err != nil {
		return err
	}
	if spec.Operation == "move" {
		present, err := sourceUnchanged(ctx, rule, settings, spec.Files, true)
		if err != nil {
			return err
		}
		if len(present) > 0 {
			cleanup := filepath.Join(jobDir, "cleanup.txt")
			if err := os.WriteFile(cleanup, []byte(strings.Join(present, "\n")+"\n"), 0o600); err != nil {
				return err
			}
			if _, err := metadataCommand(ctx, settings, "delete", sourceSpec(rule), "--files-from-raw", cleanup); err != nil {
				return err
			}
		}
		remaining, err := sourceUnchanged(ctx, rule, settings, spec.Files, true)
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			return errors.New("影片已发布，源文件清理未完成，可重试清理阶段")
		}
	}
	for _, f := range spec.Files {
		*donePaths = append(*donePaths, f.Path)
	}
	job.Phase = "completed"
	return s.st.SetTaskPhase(ctx, job.JobID, job.Phase, job.StagePath)
}

func writeTaskFileList(dir string, files []store.TransferJobFile) (string, error) {
	var b strings.Builder
	for _, f := range files {
		if !validRelativeFile(f.Path) {
			return "", errors.New("文件清单包含非法路径")
		}
		b.WriteString(f.Path)
		b.WriteByte('\n')
	}
	p := filepath.Join(dir, "files.txt")
	return p, os.WriteFile(p, []byte(b.String()), 0o600)
}
func assertCompleteSource(ctx context.Context, rule store.Rule, settings store.RuntimeSettings, files []store.TransferJobFile) error {
	if _, err := sourceUnchanged(ctx, rule, settings, files, false); err != nil {
		return err
	}
	entries, err := scanRule(ctx, rule, settings)
	if err != nil {
		return err
	}
	entries, err = store.TransferEntries(rule, entries)
	if err != nil {
		return err
	}
	if len(entries) != len(files) {
		return errors.New("影片目录在传输期间新增或移除了文件，未发布，请重新核对完整清单")
	}
	return nil
}
