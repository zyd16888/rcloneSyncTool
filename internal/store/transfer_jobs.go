package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// OriginAPI marks jobs created through /api/v1. They share the jobs table with
// scheduler and manual jobs so quota, limit groups and the job list keep
// working, but they never claim rows from the scanner file queue.
const OriginAPI = "api"

// Transfer job lifecycle. blocked is the one state the scheduler-driven path
// does not have: an API caller must be able to see that work is deferred by a
// quota instead of watching a job sit in pending forever.
const (
	TransferStatusPending    = "pending"
	TransferStatusBlocked    = "blocked"
	TransferStatusRunning    = "running"
	TransferStatusDone       = "done"
	TransferStatusFailed     = "failed"
	TransferStatusTerminated = "terminated"
)

// Block reasons are stable identifiers a client maps to its own state.
const (
	BlockReasonQuotaExhausted    = "quota_exhausted"
	BlockReasonRcloneUnavailable = "rclone_unavailable"
)

var ErrTransferJobNotFound = errors.New("transfer job not found")

// TransferJob is a job row plus the fields only API-created jobs use.
type TransferJob struct {
	JobID          string
	RuleID         string
	Origin         string
	TransferMode   string
	Status         string
	BlockReason    string
	ExternalID     string
	IdempotencyKey string
	Fingerprint    string
	RequestJSON    string
	ResultJSON     string
	CallbackURL    string
	CallbackState  string
	BytesDone      int64
	AvgSpeed       float64
	Error          string
	LogPath        string
	StartedAt      time.Time
	EndedAt        time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (j TransferJob) Terminal() bool {
	switch j.Status {
	case TransferStatusDone, TransferStatusFailed, TransferStatusTerminated:
		return true
	}
	return false
}

// TransferJobFile is one entry of the job manifest.
type TransferJobFile struct {
	JobID     string
	Path      string
	Size      int64
	State     string
	LastError string
}

const transferJobColumns = `job_id, rule_id, COALESCE(origin,''), transfer_mode, status,
COALESCE(block_reason,''), COALESCE(external_id,''), COALESCE(idempotency_key,''),
COALESCE(request_fingerprint,''), COALESCE(request_snapshot,''), COALESCE(result_snapshot,''),
COALESCE(callback_url,''), COALESCE(callback_state,''), bytes_done, avg_speed, error, log_path,
started_at, ended_at, COALESCE(created_at,0), COALESCE(updated_at,0)`

func scanTransferJob(scan func(dest ...any) error) (TransferJob, error) {
	var j TransferJob
	var started, ended, created, updated int64
	err := scan(
		&j.JobID, &j.RuleID, &j.Origin, &j.TransferMode, &j.Status,
		&j.BlockReason, &j.ExternalID, &j.IdempotencyKey,
		&j.Fingerprint, &j.RequestJSON, &j.ResultJSON,
		&j.CallbackURL, &j.CallbackState, &j.BytesDone, &j.AvgSpeed, &j.Error, &j.LogPath,
		&started, &ended, &created, &updated,
	)
	if err != nil {
		return TransferJob{}, err
	}
	j.StartedAt = time.Unix(started, 0)
	if ended != 0 {
		j.EndedAt = time.Unix(ended, 0)
	}
	if created != 0 {
		j.CreatedAt = time.Unix(created, 0)
	}
	if updated != 0 {
		j.UpdatedAt = time.Unix(updated, 0)
	}
	return j, nil
}

// CreateTransferJob inserts an API job together with its declared file list in
// one transaction. The unique indexes on idempotency_key and external_id are
// what make a duplicate submit fail here rather than start a second transfer.
func (s *Store) CreateTransferJob(ctx context.Context, job TransferJob, files []TransferJobFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := nowUnix()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO jobs(job_id, rule_id, origin, transfer_mode, rc_port, started_at, status,
  log_path, external_id, idempotency_key, request_fingerprint, request_snapshot,
  callback_url, created_at, updated_at)
VALUES(?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`,
		job.JobID, job.RuleID, OriginAPI, job.TransferMode, now, TransferStatusPending,
		job.LogPath, job.ExternalID, job.IdempotencyKey, job.Fingerprint, job.RequestJSON,
		job.CallbackURL, now, now,
	); err != nil {
		return err
	}

	if len(files) > 0 {
		stmt, err := tx.PrepareContext(ctx, `
INSERT INTO transfer_job_files(job_id, path, size, state, last_error)
VALUES(?, ?, ?, 'pending', '')
ON CONFLICT(job_id, path) DO NOTHING
`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, f := range files {
			if _, err := stmt.ExecContext(ctx, job.JobID, f.Path, f.Size); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) GetTransferJob(ctx context.Context, jobID string) (TransferJob, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+transferJobColumns+` FROM jobs WHERE job_id=?`, jobID)
	job, err := scanTransferJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return TransferJob{}, false, nil
	}
	if err != nil {
		return TransferJob{}, false, err
	}
	return job, true, nil
}

func (s *Store) GetTransferJobByExternalID(ctx context.Context, externalID string) (TransferJob, bool, error) {
	if strings.TrimSpace(externalID) == "" {
		return TransferJob{}, false, nil
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+transferJobColumns+` FROM jobs WHERE external_id=?`, externalID)
	job, err := scanTransferJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return TransferJob{}, false, nil
	}
	if err != nil {
		return TransferJob{}, false, err
	}
	return job, true, nil
}

func (s *Store) GetTransferJobByIdempotencyKey(ctx context.Context, key string) (TransferJob, bool, error) {
	if strings.TrimSpace(key) == "" {
		return TransferJob{}, false, nil
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+transferJobColumns+` FROM jobs WHERE idempotency_key=?`, key)
	job, err := scanTransferJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return TransferJob{}, false, nil
	}
	if err != nil {
		return TransferJob{}, false, err
	}
	return job, true, nil
}

// ClaimableTransferJobs lists API jobs waiting to start. blocked jobs are
// included so a job deferred by a quota resumes on its own once the window
// rolls, without the client having to resubmit.
func (s *Store) ClaimableTransferJobs(ctx context.Context, limit int) ([]TransferJob, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT `+transferJobColumns+`
FROM jobs
WHERE origin=? AND status IN (?, ?)
ORDER BY created_at ASC, job_id ASC
LIMIT ?
`, OriginAPI, TransferStatusPending, TransferStatusBlocked, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TransferJob
	for rows.Next() {
		job, err := scanTransferJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// StartTransferJob moves a job to running only if it is still waiting. The
// conditional UPDATE is what keeps two scheduler ticks from launching the same
// rclone process twice.
func (s *Store) StartTransferJob(ctx context.Context, jobID string, rcPort int) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET status=?, rc_port=?, error='', block_reason='', started_at=strftime('%s','now'), updated_at=?
WHERE job_id=? AND status IN (?, ?)
`, TransferStatusRunning, rcPort, nowUnix(), jobID, TransferStatusPending, TransferStatusBlocked)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected == 1, err
}

func (s *Store) BlockTransferJob(ctx context.Context, jobID, reason, message string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET status=?, block_reason=?, error=?, updated_at=?
WHERE job_id=? AND status IN (?, ?)
`, TransferStatusBlocked, reason, message, nowUnix(), jobID, TransferStatusPending, TransferStatusBlocked)
	return err
}

func (s *Store) FinishTransferJob(
	ctx context.Context,
	jobID string,
	status string,
	bytesDone int64,
	avgSpeed float64,
	errMsg string,
	result any,
) error {
	encoded := ""
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		encoded = string(b)
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET status=?, block_reason='', ended_at=strftime('%s','now'), bytes_done=?, avg_speed=?,
    error=?, result_snapshot=?, updated_at=?
WHERE job_id=?
`, status, bytesDone, avgSpeed, errMsg, encoded, nowUnix(), jobID)
	return err
}

func (s *Store) SetTransferJobCallbackState(ctx context.Context, jobID, state string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET callback_state=?, updated_at=? WHERE job_id=?`,
		state, nowUnix(), jobID)
	return err
}

// ── manifest files ──────────────────────────────────────────────────────────

func (s *Store) ReplaceTransferJobFiles(ctx context.Context, jobID string, files []TransferJobFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM transfer_job_files WHERE job_id=?`, jobID); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO transfer_job_files(job_id, path, size, state, last_error)
VALUES(?, ?, ?, ?, ?)
`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, f := range files {
		state := f.State
		if state == "" {
			state = "pending"
		}
		if _, err := stmt.ExecContext(ctx, jobID, f.Path, f.Size, state, f.LastError); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkTransferJobFileStates settles the manifest: paths in done become done,
// everything still pending takes remainingState.
func (s *Store) MarkTransferJobFileStates(
	ctx context.Context,
	jobID string,
	done []string,
	remainingState string,
	errMsg string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if len(done) > 0 {
		stmt, err := tx.PrepareContext(ctx, `
UPDATE transfer_job_files SET state='done', last_error='' WHERE job_id=? AND path=?
`)
		if err != nil {
			return err
		}
		for _, path := range done {
			if _, err := stmt.ExecContext(ctx, jobID, path); err != nil {
				_ = stmt.Close()
				return err
			}
		}
		_ = stmt.Close()
	}
	if remainingState != "" {
		if _, err := tx.ExecContext(ctx, `
UPDATE transfer_job_files
SET state=?, last_error=?
WHERE job_id=? AND state='pending'
`, remainingState, errMsg, jobID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListTransferJobFiles pages the manifest by path so a job with a very large
// file list never has to be serialized in one response.
func (s *Store) ListTransferJobFiles(
	ctx context.Context,
	jobID string,
	afterPath string,
	limit int,
) ([]TransferJobFile, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT job_id, path, size, state, last_error
FROM transfer_job_files
WHERE job_id=? AND path > ?
ORDER BY path ASC
LIMIT ?
`, jobID, afterPath, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TransferJobFile
	for rows.Next() {
		var f TransferJobFile
		if err := rows.Scan(&f.JobID, &f.Path, &f.Size, &f.State, &f.LastError); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

type TransferJobFileCounts struct {
	Total   int
	Done    int
	Failed  int
	Pending int
	Bytes   int64
}

func (s *Store) TransferJobFileCounts(ctx context.Context, jobID string) (TransferJobFileCounts, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT state, COUNT(*), COALESCE(SUM(size),0)
FROM transfer_job_files
WHERE job_id=?
GROUP BY state
`, jobID)
	if err != nil {
		return TransferJobFileCounts{}, err
	}
	defer rows.Close()
	var counts TransferJobFileCounts
	for rows.Next() {
		var state string
		var n int
		var bytes int64
		if err := rows.Scan(&state, &n, &bytes); err != nil {
			return TransferJobFileCounts{}, err
		}
		counts.Total += n
		switch state {
		case "done":
			counts.Done = n
			counts.Bytes += bytes
		case "failed":
			counts.Failed = n
		case "pending":
			counts.Pending = n
		}
	}
	return counts, rows.Err()
}
