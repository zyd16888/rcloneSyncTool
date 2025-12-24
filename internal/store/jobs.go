package store

import (
	"context"
	"database/sql"
	"errors"
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

func (s *Store) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT job_id, rule_id, transfer_mode, rc_port, started_at, ended_at, status, bytes_done, avg_speed, error, log_path
FROM jobs
ORDER BY started_at DESC
LIMIT ?
`, limit)
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

func (s *Store) CountRunningJobsAll(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status='running'`).Scan(&n)
	return n, err
}
