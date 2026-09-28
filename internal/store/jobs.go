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
	Origin        string
	Phase         string
	BlockReason   string
}

type JobFilter struct {
	RuleID       string
	Status       string
	TransferMode string
	Query        string
}

type RealtimeSummary struct {
	BytesTotal  int64
	SpeedTotal  float64
	RunningJobs int
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
SELECT job_id, rule_id, transfer_mode, rc_port, started_at, ended_at, status, bytes_done, avg_speed, error, log_path,origin,phase,block_reason
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
		if err := rows.Scan(&j.JobID, &j.RuleID, &j.TransferMode, &j.RcPort, &started, &ended, &j.Status, &j.BytesDone, &j.AvgSpeed, &j.Error, &j.LogPath, &j.Origin, &j.Phase, &j.BlockReason); err != nil {
			return nil, err
		}
		if started > 0 {
			j.StartedAt = time.Unix(started, 0)
		}
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
SELECT job_id, rule_id, transfer_mode, rc_port, started_at, ended_at, status, bytes_done, avg_speed, error, log_path,origin,phase,block_reason
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
		if err := rows.Scan(&j.JobID, &j.RuleID, &j.TransferMode, &j.RcPort, &started, &ended, &j.Status, &j.BytesDone, &j.AvgSpeed, &j.Error, &j.LogPath, &j.Origin, &j.Phase, &j.BlockReason); err != nil {
			return nil, err
		}
		if started > 0 {
			j.StartedAt = time.Unix(started, 0)
		}
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
SELECT job_id, rule_id, transfer_mode, rc_port, started_at, ended_at, status, bytes_done, avg_speed, error, log_path,origin,phase,block_reason
FROM jobs
WHERE job_id=?
`, id).Scan(&j.JobID, &j.RuleID, &j.TransferMode, &j.RcPort, &started, &ended, &j.Status, &j.BytesDone, &j.AvgSpeed, &j.Error, &j.LogPath, &j.Origin, &j.Phase, &j.BlockReason)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	if started > 0 {
		j.StartedAt = time.Unix(started, 0)
	}
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recordUsage(ctx, tx, m.JobID, m.Bytes, m.Ts.UnixMilli()); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT OR IGNORE INTO job_metrics(job_id, ts, bytes, speed, transfers, errors)
VALUES(?, ?, ?, ?, ?, ?)
`, m.JobID, m.Ts.UnixMilli(), m.Bytes, m.Speed, m.Transfers, m.Errors)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET reserved_bytes=MAX(0,reserved_bytes-(MAX(bytes_done,?)-bytes_done)),bytes_done=MAX(bytes_done,?),avg_speed=? WHERE job_id=? AND status='running'`, m.Bytes, m.Bytes, m.Speed, m.JobID); err != nil {
		return err
	}
	return tx.Commit()
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

func (s *Store) usageSince(ctx context.Context, scope string, value string, since time.Time) (int64, error) {
	where := ""
	args := []any{since.UnixMilli()}
	legacyWhere := ""
	legacyArgs := []any{since.Unix()}
	if scope != "" {
		where = " AND " + scope + "=?"
		args = append(args, value)
		legacyWhere = " AND " + scope + "=?"
		legacyArgs = append(legacyArgs, value)
	}
	var usage, legacy int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM transfer_usage WHERE ts>=?`+where, args...).Scan(&usage); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes_done),0) FROM jobs j WHERE ended_at>=? AND status NOT IN ('pending','blocked','running')`+legacyWhere+` AND NOT EXISTS(SELECT 1 FROM transfer_usage u WHERE u.job_id=j.job_id)`, legacyArgs...).Scan(&legacy); err != nil {
		return 0, err
	}
	return usage + legacy, nil
}
func (s *Store) StatsBytesSince(ctx context.Context, since time.Time) (int64, error) {
	return s.usageSince(ctx, "", "", since)
}
func (s *Store) RuleUsageSince(ctx context.Context, ruleID string, since time.Time) (int64, error) {
	return s.usageSince(ctx, "rule_id", ruleID, since)
}
func (s *Store) GroupUsageSince(ctx context.Context, group string, since time.Time) (int64, error) {
	return s.usageSince(ctx, "quota_group", group, since)
}
func (s *Store) budgetSince(ctx context.Context, scope, value string, since time.Time) (int64, error) {
	usage, err := s.usageSince(ctx, scope, value, since)
	if err != nil {
		return 0, err
	}
	var reserved int64
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_bytes),0) FROM jobs WHERE status='running' AND `+scope+`=?`, value).Scan(&reserved)
	return usage + reserved, err
}
func (s *Store) RuleBudgetSince(ctx context.Context, ruleID string, since time.Time) (int64, error) {
	return s.budgetSince(ctx, "rule_id", ruleID, since)
}
func (s *Store) GroupBudgetSince(ctx context.Context, group string, since time.Time) (int64, error) {
	return s.budgetSince(ctx, "quota_group", group, since)
}
func (s *Store) CountRunningJobsAll(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status='running'`).Scan(&n)
	return n, err
}
