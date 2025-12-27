package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type Job struct {
	JobID         string
	RuleID        string
	TransferMode  string
	RcPort        int
	StartedAt     time.Time
	EndedAt       time.Time
	Status        string
	BytesDone     int64
	AvgSpeed      float64
	Error         string
	LogPath       string
	SelectedFiles int
}

type JobFilter struct {
	RuleID       string
	Status       string
	TransferMode string
	Query        string
}

type RealtimeSummary struct {
	BytesTotal   int64
	SpeedTotal   float64
	RunningJobs  int
}

func (s *Store) RealtimeSummary(ctx context.Context, ruleID string) (RealtimeSummary, error) {
	if strings.TrimSpace(ruleID) == "" {
		var bytes int64
		var speed float64
		var running int
		if err := s.db.QueryRowContext(ctx, `
SELECT
  (SELECT COALESCE(SUM(bytes_done),0) FROM jobs),
  (SELECT COALESCE(SUM(avg_speed),0) FROM jobs WHERE status='running'),
  (SELECT COUNT(*) FROM jobs WHERE status='running')
`).Scan(&bytes, &speed, &running); err != nil {
			return RealtimeSummary{}, err
		}
		return RealtimeSummary{BytesTotal: bytes, SpeedTotal: speed, RunningJobs: running}, nil
	}
	var bytes int64
	var speed float64
	var running int
	if err := s.db.QueryRowContext(ctx, `
SELECT
  (SELECT COALESCE(SUM(bytes_done),0) FROM jobs WHERE rule_id=?),
  (SELECT COALESCE(SUM(avg_speed),0) FROM jobs WHERE rule_id=? AND status='running'),
  (SELECT COUNT(*) FROM jobs WHERE rule_id=? AND status='running')
`, ruleID, ruleID, ruleID).Scan(&bytes, &speed, &running); err != nil {
		return RealtimeSummary{}, err
	}
	return RealtimeSummary{BytesTotal: bytes, SpeedTotal: speed, RunningJobs: running}, nil
}

func (s *Store) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	return s.ListJobsPage(ctx, limit, 0)
}

func (s *Store) ListJobsPage(ctx context.Context, limit, offset int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT job_id, rule_id, transfer_mode, rc_port, started_at, ended_at, status, bytes_done, avg_speed, error, log_path
FROM jobs
ORDER BY started_at DESC
LIMIT ? OFFSET ?
`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var started, ended int64
		if err := rows.Scan(&j.JobID, &j.RuleID, &j.TransferMode, &j.RcPort, &started, &ended, &j.Status, &j.BytesDone, &j.AvgSpeed, &j.Error, &j.LogPath); err != nil {
			return nil, err
		}
		j.StartedAt = time.Unix(started, 0)
		if ended != 0 {
			j.EndedAt = time.Unix(ended, 0)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) CountJobs(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&n)
	return n, err
}

func (s *Store) ListJobsPageFiltered(ctx context.Context, limit, offset int, f JobFilter) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	where, args := buildJobsWhere(f)
	q := `
SELECT job_id, rule_id, transfer_mode, rc_port, started_at, ended_at, status, bytes_done, avg_speed, error, log_path
FROM jobs
` + where + `
ORDER BY started_at DESC
LIMIT ? OFFSET ?
`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var started, ended int64
		if err := rows.Scan(&j.JobID, &j.RuleID, &j.TransferMode, &j.RcPort, &started, &ended, &j.Status, &j.BytesDone, &j.AvgSpeed, &j.Error, &j.LogPath); err != nil {
			return nil, err
		}
		j.StartedAt = time.Unix(started, 0)
		if ended != 0 {
			j.EndedAt = time.Unix(ended, 0)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) CountJobsFiltered(ctx context.Context, f JobFilter) (int, error) {
	where, args := buildJobsWhere(f)
	q := `SELECT COUNT(*) FROM jobs` + where
	var n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

func buildJobsWhere(f JobFilter) (string, []any) {
	var b strings.Builder
	var args []any
	b.WriteString("WHERE 1=1\n")
	if strings.TrimSpace(f.RuleID) != "" {
		b.WriteString(" AND rule_id=?\n")
		args = append(args, strings.TrimSpace(f.RuleID))
	}
	if strings.TrimSpace(f.Status) != "" {
		b.WriteString(" AND status=?\n")
		args = append(args, strings.TrimSpace(f.Status))
	}
	if strings.TrimSpace(f.TransferMode) != "" {
		b.WriteString(" AND transfer_mode=?\n")
		args = append(args, strings.TrimSpace(f.TransferMode))
	}
	if strings.TrimSpace(f.Query) != "" {
		b.WriteString(" AND (job_id LIKE ? OR error LIKE ?)\n")
		kw := "%" + strings.TrimSpace(f.Query) + "%"
		args = append(args, kw, kw)
	}
	return "\n" + strings.TrimSpace(b.String()) + "\n", args
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, bool, error) {
	var j Job
	var started, ended int64
	err := s.db.QueryRowContext(ctx, `
SELECT job_id, rule_id, transfer_mode, rc_port, started_at, ended_at, status, bytes_done, avg_speed, error, log_path
FROM jobs
WHERE job_id=?
`, id).Scan(&j.JobID, &j.RuleID, &j.TransferMode, &j.RcPort, &started, &ended, &j.Status, &j.BytesDone, &j.AvgSpeed, &j.Error, &j.LogPath)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	j.StartedAt = time.Unix(started, 0)
	if ended != 0 {
		j.EndedAt = time.Unix(ended, 0)
	}
	return j, true, nil
}

type JobMetric struct {
	JobID     string
	Ts        time.Time
	Bytes     int64
	Speed     float64
	Transfers int
	Errors    int
}

func (s *Store) LatestJobMetric(ctx context.Context, jobID string) (JobMetric, bool, error) {
	var m JobMetric
	var ts int64
	err := s.db.QueryRowContext(ctx, `
SELECT job_id, ts, bytes, speed, transfers, errors
FROM job_metrics
WHERE job_id=?
ORDER BY ts DESC
LIMIT 1
`, jobID).Scan(&m.JobID, &ts, &m.Bytes, &m.Speed, &m.Transfers, &m.Errors)
	if errors.Is(err, sql.ErrNoRows) {
		return JobMetric{}, false, nil
	}
	if err != nil {
		return JobMetric{}, false, err
	}
	m.Ts = time.UnixMilli(ts)
	return m, true, nil
}

func (s *Store) InsertJobMetric(ctx context.Context, m JobMetric) error {
	_, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO job_metrics(job_id, ts, bytes, speed, transfers, errors)
VALUES(?, ?, ?, ?, ?, ?)
`, m.JobID, m.Ts.UnixMilli(), m.Bytes, m.Speed, m.Transfers, m.Errors)
	return err
}

func (s *Store) UpdateJobRunningStats(ctx context.Context, jobID string, bytesDone int64, speed float64) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET bytes_done=?, avg_speed=?
WHERE job_id=? AND status='running'
`, bytesDone, speed, jobID)
	return err
}

func (s *Store) TotalBytesDone(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes_done),0) FROM jobs`).Scan(&n)
	return n, err
}

func (s *Store) TotalSpeedRunning(ctx context.Context) (float64, error) {
	var n float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(avg_speed),0) FROM jobs WHERE status='running'`).Scan(&n)
	return n, err
}

func (s *Store) StatsBytesSince(ctx context.Context, since time.Time) (int64, error) {
	// Sum bytes of:
	// 1. Jobs ended after 'since' (status != 'running')
	// 2. Jobs currently running (status == 'running'), regardless of start time (treat all current progress as active)
	q := `
SELECT COALESCE(SUM(bytes_done), 0)
FROM jobs
WHERE (status = 'running')
   OR (ended_at >= ? AND status != 'running')
`
	var n int64
	err := s.db.QueryRowContext(ctx, q, since.Unix()).Scan(&n)
	return n, err
}

func (s *Store) RuleUsageSince(ctx context.Context, ruleID string, since time.Time) (int64, error) {
	q := `
SELECT COALESCE(SUM(bytes_done), 0)
FROM jobs
WHERE rule_id = ?
  AND ((status = 'running') OR (ended_at >= ? AND status != 'running'))
`
	var n int64
	err := s.db.QueryRowContext(ctx, q, ruleID, since.Unix()).Scan(&n)
	return n, err
}

// RuleBudgetSince returns an estimated usage for scheduling decisions:
// - counts bytes_done for ended jobs in the window
// - counts full size of currently transferring files (in-flight reservation)
// This avoids starting new jobs when concurrent running jobs would cause quota overshoot.
func (s *Store) RuleBudgetSince(ctx context.Context, ruleID string, since time.Time) (int64, error) {
	var ended int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(bytes_done), 0)
FROM jobs
WHERE rule_id = ?
  AND ended_at >= ?
  AND status != 'running'
`, ruleID, since.Unix()).Scan(&ended); err != nil {
		return 0, err
	}

	var inflight int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(size), 0)
FROM files
WHERE rule_id = ?
  AND state = 'transferring'
`, ruleID).Scan(&inflight); err != nil {
		return 0, err
	}
	return ended + inflight, nil
}

func (s *Store) GroupUsageSince(ctx context.Context, group string, since time.Time) (int64, error) {
	if group == "" {
		return 0, nil
	}
	q := `
SELECT COALESCE(SUM(j.bytes_done), 0)
FROM jobs j
JOIN rules r ON j.rule_id = r.id
WHERE r.limit_group = ?
  AND ((j.status = 'running') OR (j.ended_at >= ? AND j.status != 'running'))
`
	var n int64
	err := s.db.QueryRowContext(ctx, q, group, since.Unix()).Scan(&n)
	return n, err
}

// GroupBudgetSince is the group-level variant of RuleBudgetSince.
func (s *Store) GroupBudgetSince(ctx context.Context, group string, since time.Time) (int64, error) {
	if group == "" {
		return 0, nil
	}
	var ended int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(j.bytes_done), 0)
FROM jobs j
JOIN rules r ON j.rule_id = r.id
WHERE r.limit_group = ?
  AND j.ended_at >= ?
  AND j.status != 'running'
`, group, since.Unix()).Scan(&ended); err != nil {
		return 0, err
	}

	var inflight int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(f.size), 0)
FROM files f
JOIN rules r ON f.rule_id = r.id
WHERE r.limit_group = ?
  AND f.state = 'transferring'
`, group).Scan(&inflight); err != nil {
		return 0, err
	}
	return ended + inflight, nil
}

func (s *Store) CountRunningJobsAll(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status='running'`).Scan(&n)
	return n, err
}
