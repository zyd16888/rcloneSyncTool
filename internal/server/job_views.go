package server

import (
	"context"
	"encoding/json"
	"math"
	"path"
	"path/filepath"
	"strings"
	"time"

	"115togd/internal/daemon"
	"115togd/internal/store"
)

type jobStep struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type jobView struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	RuleID        string    `json:"rule_id"`
	RuleLabel     string    `json:"rule_label"`
	Origin        string    `json:"origin"`
	Mode          string    `json:"mode"`
	Status        string    `json:"status"`
	StatusLabel   string    `json:"status_label"`
	Phase         string    `json:"phase"`
	Source        string    `json:"source"`
	Destination   string    `json:"destination"`
	StagePath     string    `json:"stage_path"`
	Error         string    `json:"error"`
	RetryOf       string    `json:"retry_of"`
	Terminal      bool      `json:"terminal"`
	CanStop       bool      `json:"can_stop"`
	CanRetry      bool      `json:"can_retry"`
	RetryPending  bool      `json:"retry_pending"`
	ProgressKnown bool      `json:"progress_known"`
	PlannedBytes  int64     `json:"planned_bytes"`
	ReadyBytes    int64     `json:"ready_bytes"`
	ActualBytes   int64     `json:"actual_bytes"`
	ReusedBytes   int64     `json:"reused_bytes"`
	Percent       float64   `json:"percent"`
	Speed         float64   `json:"speed"`
	AverageSpeed  float64   `json:"average_speed"`
	ETA           int64     `json:"eta"`
	FilesTotal    int       `json:"files_total"`
	FilesDone     int       `json:"files_done"`
	FilesFailed   int       `json:"files_failed"`
	CreatedAt     time.Time `json:"created_at"`
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at"`
	Steps         []jobStep `json:"steps"`
}

func (s *Server) metricFreshSince(ctx context.Context) time.Time {
	interval := 15 * time.Second
	if settings, err := s.st.RuntimeSettings(ctx); err == nil {
		interval = max(interval, 3*settings.MetricsInterval)
	}
	return time.Now().Add(-interval)
}

func (s *Server) jobViews(ctx context.Context, jobs []store.TransferJob) ([]jobView, error) {
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.JobID
	}
	files, err := s.st.JobFileSummaries(ctx, ids)
	if err != nil {
		return nil, err
	}
	metrics, err := s.st.JobMetricsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	retries, err := s.st.ActiveRetriesFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	rules, err := s.st.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	ruleMap := map[string]store.Rule{}
	for _, r := range rules {
		ruleMap[r.ID] = r
	}
	fresh := s.metricFreshSince(ctx)
	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, s.makeJobView(j, files[j.JobID], metrics[j.JobID], retries[j.JobID], ruleMap[j.RuleID], fresh))
	}
	return out, nil
}

func (s *Server) makeJobView(j store.TransferJob, files store.JobFileSummary, metric store.JobMetric, retryPending bool, rule store.Rule, fresh time.Time) jobView {
	v := jobView{ID: j.JobID, RuleID: j.RuleID, RuleLabel: j.RuleID, Origin: j.Origin, Mode: j.TransferMode,
		Status: j.Status, StatusLabel: statusName(j.Status), Phase: j.Phase, Error: redactLogLine(j.Error), RetryOf: j.RetryOf,
		Terminal: j.Terminal(), CanStop: !j.Terminal(), RetryPending: retryPending, ActualBytes: j.BytesDone, AverageSpeed: j.AvgSpeed,
		FilesTotal: files.Total, FilesDone: files.Done, FilesFailed: files.Failed, CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, EndedAt: j.EndedAt, StagePath: j.StagePath}
	spec, _ := daemon.DecodeStoredTransferSpec(j.RequestJSON)
	if spec.RuleSnapshot != nil {
		rule = *spec.RuleSnapshot
	}
	if j.Origin == store.OriginManual || strings.HasPrefix(j.RuleID, "manual_") {
		v.RuleLabel = "手动运行"
	}
	v.Source = sourcePath(rule)
	v.Destination = destinationPath(rule)
	if spec.SourceSubpath != "" {
		if rule.SrcKind == "local" {
			v.Source = filepath.Join(rule.SrcLocalRoot, filepath.FromSlash(spec.SourceSubpath))
		} else {
			v.Source = strings.TrimSuffix(v.Source, "/") + "/" + spec.SourceSubpath
		}
	}
	if spec.DestinationSubpath != "" {
		v.Destination = strings.TrimSuffix(v.Destination, "/") + "/" + spec.DestinationSubpath
	}
	v.Title = taskTitle(j, files, spec, rule)
	v.CanRetry = (j.Status == "failed" || j.Status == "terminated") && j.RequestJSON != "" && !retryPending
	if j.Status == "running" {
		if j.Phase == "copying" || j.Phase == "" {
			if !metric.Ts.IsZero() && metric.Ts.After(fresh) {
				v.Speed = max(0, metric.Speed)
			}
		}
		if name := phaseName(j.Phase); name != "" {
			v.StatusLabel = name
		}
	}
	if j.Status == "blocked" {
		v.StatusLabel = blockLabel(j.BlockReason)
	}
	p := daemon.TaskProgress{}
	if s.supervisor != nil && j.Status == "running" {
		p = s.supervisor.JobProgress(j.JobID, j.BytesDone, false)
	}
	if !p.Initialized && j.ResultJSON != "" {
		var result struct {
			Progress daemon.TaskProgress `json:"progress"`
		}
		if json.Unmarshal([]byte(j.ResultJSON), &result) == nil {
			p = result.Progress
		}
	}
	v.ProgressKnown = j.Prepared && files.Bytes > 0
	v.PlannedBytes = files.Bytes
	if p.Initialized {
		v.ProgressKnown = p.PlannedBytes > 0
		v.PlannedBytes = p.PlannedBytes
		v.ReadyBytes = p.ReadyBytes
		v.ReusedBytes = p.ReusedBytes
	} else if v.ProgressKnown {
		v.ReadyBytes = min(v.PlannedBytes, j.BytesDone)
	}
	if j.Status == "done" || (j.Status == "running" && (j.Phase == "verifying" || j.Phase == "publishing" || j.Phase == "cleanup" || j.Phase == "completed")) {
		v.ReadyBytes = v.PlannedBytes
	}
	if v.PlannedBytes > 0 {
		v.ReadyBytes = max(int64(0), min(v.PlannedBytes, v.ReadyBytes))
		v.Percent = float64(v.ReadyBytes) * 100 / float64(v.PlannedBytes)
		if v.Speed > 0 && j.Phase == "copying" && v.ReadyBytes < v.PlannedBytes {
			v.ETA = int64(math.Ceil(float64(v.PlannedBytes-v.ReadyBytes) / v.Speed))
		}
	}
	v.Steps = taskSteps(j, rule.AtomicPublish)
	return v
}

func taskTitle(job store.TransferJob, files store.JobFileSummary, spec daemon.TransferSpec, rule store.Rule) string {
	if spec.SourceSubpath != "" {
		return path.Base(strings.TrimSuffix(spec.SourceSubpath, "/"))
	}
	if job.GroupKey != "" {
		_, label, found := strings.Cut(job.GroupKey, ":")
		if found && label != "" {
			return path.Base(label)
		}
	}
	if files.Total == 1 && files.FirstPath != "" {
		return path.Base(files.FirstPath)
	}
	if job.Origin == store.OriginManual {
		if rule.SrcKind == "local" {
			return filepath.Base(rule.SrcLocalRoot)
		}
		if rule.SrcPath != "" && rule.SrcPath != "/" {
			return path.Base(rule.SrcPath)
		}
		return "手动传输"
	}
	return job.RuleID
}

func taskSteps(job store.TransferJob, atomic bool) []jobStep {
	names := []string{"准备", "传输", "校验"}
	phases := []string{"preparing", "copying", "verifying"}
	if atomic {
		names = append(names, "发布")
		phases = append(phases, "publishing")
		if job.TransferMode == "move" {
			names = append(names, "清理")
			phases = append(phases, "cleanup")
		}
	}
	current := 0
	for i, p := range phases {
		if p == job.Phase {
			current = i
		}
	}
	if job.Status == "done" || job.Phase == "completed" {
		current = len(phases)
	}
	out := make([]jobStep, len(names))
	for i, name := range names {
		state := ""
		if i < current {
			state = "is-complete"
		} else if i == current {
			if job.Status == "failed" || job.Status == "terminated" {
				state = "is-error"
			} else if job.Status == "running" {
				state = "is-active"
			}
		}
		out[i] = jobStep{Name: name, State: state}
	}
	return out
}

func blockLabel(reason string) string {
	switch reason {
	case "quota_exhausted":
		return "等待配额"
	case "rclone_unavailable":
		return "等待 rclone"
	case "rule_paused":
		return "调度已暂停"
	case "concurrency", "rc_port":
		return "等待并发名额"
	case "source_busy":
		return "等待同组任务"
	default:
		return "等待条件恢复"
	}
}
