package store

import (
	"context"
	// "database/sql"
	"errors"
	"time"
)

type FileStateCounts struct {
	New          int
	Stable       int
	Queued       int
	Transferring int
	Done         int
	Failed       int
	Missing      int
}

func (s *Store) RuleFileCounts(ctx context.Context, ruleID string) (FileStateCounts, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT state, COUNT(*)
FROM files
WHERE rule_id=?
GROUP BY state
`, ruleID)
	if err != nil {
		return FileStateCounts{}, err
	}
	defer rows.Close()
	var c FileStateCounts
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return FileStateCounts{}, err
		}
		switch st {
		case "new":
			c.New = n
		case "stable":
			c.Stable = n
		case "queued":
			c.Queued = n
		case "transferring":
			c.Transferring = n
		case "done":
			c.Done = n
		case "failed":
			c.Failed = n
		case "missing":
			c.Missing = n
		}
	}
	return c, rows.Err()
}

type ScanEntry struct {
	Path    string
	Size    int64
	ModTime time.Time
}

func (s *Store) UpsertScanEntries(ctx context.Context, rule Rule, entries []ScanEntry) error {
	_, err := s.ApplyScan(ctx, rule, entries)
	return err
}
func (s *Store) EnqueueStable(ctx context.Context, ruleID string, limit int, minSizeBytes int64) (int64, error) {
	if limit <= 0 {
		limit = 100
	}
	res, err := s.db.ExecContext(ctx, `
WITH cte AS (
  SELECT rowid
  FROM files
WHERE rule_id=? AND state='stable' AND source_present=1
 AND EXISTS(SELECT 1 FROM file_groups g JOIN rules r ON r.id=g.rule_id
 WHERE g.rule_id=files.rule_id AND g.group_key=files.group_key AND g.ready=1
 AND g.changed_at<=? - r.stable_seconds*1000)
 AND NOT EXISTS(SELECT 1 FROM files other WHERE other.rule_id=files.rule_id AND other.group_key=files.group_key AND other.source_present=1 AND other.state IN ('new','failed'))
  ORDER BY path
  LIMIT ?
)
UPDATE files
SET state='queued'
WHERE rule_id=? AND state='stable' AND source_present=1 AND group_key IN (SELECT group_key FROM files WHERE rowid IN (SELECT rowid FROM cte))
`, ruleID, time.Now().UnixMilli(), limit, ruleID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) HasQueued(ctx context.Context, ruleID string) bool {
	var one int
	err := s.db.QueryRowContext(ctx, `
SELECT 1
FROM files
WHERE rule_id=? AND state='queued' AND source_present=1 AND (job_id IS NULL OR job_id='')
LIMIT 1
`, ruleID).Scan(&one)
	return err == nil && one == 1
}

func (s *Store) RetryFailed(ctx context.Context, ruleID string, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	res, err := s.db.ExecContext(ctx, `
WITH cte AS (
  SELECT rowid
  FROM files
  WHERE rule_id=? AND state='failed' AND source_present=1
  ORDER BY last_seen DESC
  LIMIT ?
)
UPDATE files
SET state='stable', last_error='', job_id=NULL
WHERE rowid IN (SELECT rowid FROM cte)
`, ruleID, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) ClaimQueuedForJob(ctx context.Context, rule Rule, jobID string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = rule.BatchSize
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
SELECT path
FROM files
WHERE rule_id=? AND state='queued' AND (job_id IS NULL OR job_id='') AND ( ?<=0 OR size>=? )
ORDER BY last_seen DESC
LIMIT ?
`, rule.ID, rule.MinFileSizeBytes, rule.MinFileSizeBytes, limit)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return nil, err
		}
		paths = append(paths, p)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, tx.Commit()
	}

	for _, p := range paths {
		if _, err := tx.ExecContext(ctx, `
UPDATE files
SET state='transferring', job_id=?
WHERE rule_id=? AND path=? AND state='queued'
`, jobID, rule.ID, p); err != nil {
			return nil, err
		}
	}
	return paths, tx.Commit()
}

func (s *Store) GetJobFilesSize(ctx context.Context, jobID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(size), 0)
FROM files
WHERE job_id=?
`, jobID).Scan(&n)
	return n, err
}

func (s *Store) MarkJobFiles(ctx context.Context, jobID, state string, errMsg string) error {
	if state != "done" && state != "failed" {
		return errors.New("invalid file state: " + state)
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE files
SET state=?,
    last_error=CASE WHEN ?='failed' THEN ? ELSE '' END,
    fail_count=CASE WHEN ?='failed' THEN fail_count+1 ELSE fail_count END,
    error_ignored=0
WHERE job_id=?
`, state, state, errMsg, state, jobID)
	return err
}

// FinalizeJobFiles marks some paths as done, and updates remaining transferring files
// of the job to either queued or failed.
func (s *Store) FinalizeJobFiles(ctx context.Context, jobID string, donePaths []string, remainingState string, errMsg string) error {
	if remainingState != "queued" && remainingState != "failed" {
		return errors.New("invalid remaining state: " + remainingState)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if len(donePaths) > 0 {
		stmt, err := tx.PrepareContext(ctx, `
UPDATE files
SET state='done', last_error=''
WHERE job_id=? AND path=?
`)
		if err != nil {
			return err
		}
		for _, p := range donePaths {
			if _, err := stmt.ExecContext(ctx, jobID, p); err != nil {
				_ = stmt.Close()
				return err
			}
		}
		_ = stmt.Close()
	}

	switch remainingState {
	case "queued":
		if _, err := tx.ExecContext(ctx, `
UPDATE files
SET state='queued', job_id=NULL, last_error=''
WHERE job_id=? AND state='transferring'
`, jobID); err != nil {
			return err
		}
	case "failed":
		if _, err := tx.ExecContext(ctx, `
UPDATE files
SET state='failed', last_error=?, fail_count=fail_count+1,error_ignored=0
WHERE job_id=? AND state='transferring'
`, errMsg, jobID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) ReleaseTransferringBackToQueued(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE files
SET state='queued', job_id=NULL
WHERE job_id=? AND state='transferring'
`, jobID)
	return err
}

func (s *Store) ClearJobOnDone(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE files
SET job_id=NULL
WHERE job_id=? AND state='done'
`, jobID)
	return err
}

func (s *Store) CountRunningJobs(ctx context.Context, ruleID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE rule_id=? AND status='running'`, ruleID).Scan(&n)
	return n, err
}

func (s *Store) CreateJobRow(ctx context.Context, j Job) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO jobs(job_id, rule_id, transfer_mode, rc_port, started_at, status, log_path)
VALUES(?, ?, ?, ?, ?, 'running', ?)
`, j.JobID, j.RuleID, j.TransferMode, j.RcPort, j.StartedAt.Unix(), j.LogPath)
	return err
}

func (s *Store) CreateJobRowPending(ctx context.Context, j Job) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO jobs(job_id, rule_id, transfer_mode, rc_port, started_at, status, log_path)
VALUES(?, ?, ?, 0, ?, 'pending', ?)
`, j.JobID, j.RuleID, j.TransferMode, j.StartedAt.Unix(), j.LogPath)
	return err
}

func (s *Store) UpdateJobRunning(ctx context.Context, jobID string, rcPort int) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET status='running', rc_port=?, error=''
WHERE job_id=? AND status='pending'
`, rcPort, jobID)
	return err
}

func (s *Store) UpdateJobDone(ctx context.Context, jobID string, bytesDone int64, avgSpeed float64) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET status='done', ended_at=?, bytes_done=?, avg_speed=?
WHERE job_id=?
`, nowUnix(), bytesDone, avgSpeed, jobID)
	return err
}

func (s *Store) UpdateJobFailed(ctx context.Context, jobID, errMsg string, bytesDone int64, avgSpeed float64) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET status='failed', ended_at=?, error=?, error_ignored=0, bytes_done=?, avg_speed=?
WHERE job_id=?
`, nowUnix(), errMsg, bytesDone, avgSpeed, jobID)
	return err
}

func (s *Store) UpdateJobTerminated(ctx context.Context, jobID, reason string, bytesDone int64, avgSpeed float64) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET status='terminated', ended_at=?, error=?, bytes_done=?, avg_speed=?
WHERE job_id=?
`, nowUnix(), reason, bytesDone, avgSpeed, jobID)
	return err
}
