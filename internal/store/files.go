package store

import (
	"context"
	// "database/sql"
	"errors"
	"time"
)

type FileStateCounts struct {
	New         int
	Stable      int
	Queued      int
	Transferring int
	Done        int
	Failed      int
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	stableSeconds := rule.StableSeconds
	if stableSeconds < 0 {
		stableSeconds = 0
	}

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO files(rule_id, path, size, mod_time, state, last_seen, seen_size, seen_mod_time, job_id, fail_count, last_error)
VALUES(?, ?, ?, ?, ?, ?, 0, '', NULL, 0, '')
ON CONFLICT(rule_id, path) DO UPDATE SET
  seen_size=files.size,
  seen_mod_time=files.mod_time,
  size=excluded.size,
  mod_time=excluded.mod_time,
  last_seen=excluded.last_seen,
  state=CASE
    WHEN files.state='transferring' THEN files.state
    WHEN files.state='queued' THEN files.state
    WHEN files.state='done' AND (excluded.size!=files.size OR excluded.mod_time!=files.mod_time) THEN 'new'
    WHEN (excluded.size=files.size AND excluded.mod_time=files.mod_time) THEN 'stable'
    WHEN (strftime('%s','now') - strftime('%s', excluded.mod_time) > ?) THEN 'stable'
    ELSE 'new'
  END
`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		mod := e.ModTime.UTC().Format(time.RFC3339)
		initialState := "new"
		if time.Since(e.ModTime) > time.Duration(stableSeconds)*time.Second {
			initialState = "stable"
		}
		if _, err := stmt.ExecContext(ctx, rule.ID, e.Path, e.Size, mod, initialState, now, stableSeconds); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) EnqueueStable(ctx context.Context, ruleID string, limit int) (int64, error) {
	if limit <= 0 {
		limit = 100
	}
	res, err := s.db.ExecContext(ctx, `
WITH cte AS (
  SELECT rowid
  FROM files
  WHERE rule_id=? AND state='stable'
  ORDER BY last_seen DESC
  LIMIT ?
)
UPDATE files
SET state='queued'
WHERE rowid IN (SELECT rowid FROM cte)
`, ruleID, limit)
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
WHERE rule_id=? AND state='queued'
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
  WHERE rule_id=? AND state='failed'
  ORDER BY last_seen DESC
  LIMIT ?
)
UPDATE files
SET state='queued', last_error='', job_id=NULL
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
WHERE rule_id=? AND state='queued' AND (job_id IS NULL OR job_id='')
ORDER BY last_seen DESC
LIMIT ?
`, rule.ID, limit)
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

func (s *Store) MarkJobFiles(ctx context.Context, jobID, state string, errMsg string) error {
	if state != "done" && state != "failed" {
		return errors.New("invalid file state: " + state)
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE files
SET state=?,
    last_error=CASE WHEN ?='failed' THEN ? ELSE '' END,
    fail_count=CASE WHEN ?='failed' THEN fail_count+1 ELSE fail_count END
WHERE job_id=?
`, state, state, errMsg, state, jobID)
	return err
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
SET status='failed', ended_at=?, error=?, bytes_done=?, avg_speed=?
WHERE job_id=?
`, nowUnix(), errMsg, bytesDone, avgSpeed, jobID)
	return err
}
