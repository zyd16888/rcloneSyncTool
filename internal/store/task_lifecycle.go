package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrQuotaUnavailable = errors.New("quota exhausted")
var ErrTaskNotWaiting = errors.New("task is no longer waiting")
var ErrGroupBusy = errors.New("影片组已有等待或执行任务")

func (s *Store) SavePreparedTask(ctx context.Context, jobID, request string, files []TransferJobFile, groupKey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range files {
		if f.GroupKey != "" {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs j WHERE j.job_id<>? AND j.rule_id=(SELECT rule_id FROM jobs WHERE job_id=?) AND j.status IN ('pending','blocked','running') AND (j.group_key=? OR EXISTS(SELECT 1 FROM transfer_job_files tf WHERE tf.job_id=j.job_id AND tf.group_key=?))`, jobID, jobID, f.GroupKey, f.GroupKey).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return ErrGroupBusy
			}
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET request_snapshot=?,prepared=1,group_key=?,updated_at=? WHERE job_id=? AND status IN ('pending','blocked','running')`, request, groupKey, nowUnix(), jobID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTaskNotWaiting
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM transfer_job_files WHERE job_id=?`, jobID); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO transfer_job_files(job_id,path,size,state,last_error,mod_time,group_key,source_path) VALUES(?,?,?,'pending','',?,?,?)`, jobID, f.Path, f.Size, f.ModTime, f.GroupKey, f.SourcePath); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET job_id=?,state='queued',last_error='' WHERE rule_id=(SELECT rule_id FROM jobs WHERE job_id=? AND origin='scheduler') AND path=? AND (job_id IS NULL OR job_id='' OR job_id=?)`, jobID, jobID, f.SourcePath, jobID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ReserveAndStartTask(ctx context.Context, jobID string, port int, rule Rule, bytes int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE job_id=?`, jobID).Scan(&status); err != nil {
		return err
	}
	if status != "pending" && status != "blocked" && status != "running" {
		return ErrTaskNotWaiting
	}
	limit := rule.DailyLimitBytes
	where := `rule_id=?`
	scope := rule.ID
	if rule.LimitGroup != "" {
		if err := tx.QueryRowContext(ctx, `SELECT daily_limit_bytes FROM limit_groups WHERE name=?`, rule.LimitGroup).Scan(&limit); err != nil {
			return errors.New("限流分组不存在，请重新配置规则")
		}
		where, scope = `quota_group=?`, rule.LimitGroup
	}
	if limit > 0 {
		var used, reserved int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM transfer_usage WHERE `+where+` AND ts>=?`, scope, time.Now().Add(-24*time.Hour).UnixMilli()).Scan(&used); err != nil {
			return err
		}
		// Legacy tasks have no usage ledger. Count them at their recorded end
		// time until their 24-hour window expires after the upgrade.
		var legacy int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes_done),0) FROM jobs j WHERE `+where+` AND ended_at>=? AND status NOT IN ('pending','blocked','running') AND NOT EXISTS(SELECT 1 FROM transfer_usage u WHERE u.job_id=j.job_id)`, scope, time.Now().Add(-24*time.Hour).Unix()).Scan(&legacy); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_bytes),0) FROM jobs WHERE `+where+` AND status='running' AND job_id<>?`, scope, jobID).Scan(&reserved); err != nil {
			return err
		}
		if bytes > limit || used+legacy+reserved > limit-bytes {
			return ErrQuotaUnavailable
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='running',rc_port=?,reserved_bytes=?,quota_group=?,started_at=CASE WHEN started_at=0 THEN ? ELSE started_at END,block_reason='',error='',updated_at=?,queue_next_at=0 WHERE job_id=?`, port, bytes, rule.LimitGroup, nowUnix(), nowUnix(), jobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET state='transferring' WHERE job_id=?`, jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClaimTaskPreparation(ctx context.Context, jobID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='running',phase=CASE WHEN phase IN ('verifying','publishing','cleanup') THEN phase ELSE 'preparing' END,updated_at=?,block_reason='',error='' WHERE job_id=? AND status IN ('pending','blocked')`, nowUnix(), jobID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) DeferPreparingTask(ctx context.Context, jobID, reason, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='blocked',rc_port=0,reserved_bytes=0,block_reason=?,error=?,queue_next_at=?,updated_at=? WHERE job_id=? AND status='running'`, reason, message, nowUnix()+10, nowUnix(), jobID)
	return err
}

func (s *Store) CancelWaitingTask(ctx context.Context, jobID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET status='terminated',ended_at=?,updated_at=?,error='cancelled before start',reserved_bytes=0 WHERE job_id=? AND status IN ('pending','blocked')`, nowUnix(), nowUnix(), jobID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET state='failed',job_id=NULL,last_error='任务在开始前被取消' WHERE job_id=?`, jobID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) SetTaskPhase(ctx context.Context, jobID, phase, stagePath string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET phase=?,stage_path=?,updated_at=? WHERE job_id=? AND status='running'`, phase, stagePath, nowUnix(), jobID)
	return err
}

func recordUsage(ctx context.Context, tx *sql.Tx, jobID string, bytes int64, at int64) error {
	var before int64
	var ruleID, group string
	if err := tx.QueryRowContext(ctx, `SELECT bytes_done,rule_id,COALESCE(quota_group,'') FROM jobs WHERE job_id=?`, jobID).Scan(&before, &ruleID, &group); err != nil {
		return err
	}
	delta := max(int64(0), bytes-before)
	_, err := tx.ExecContext(ctx, `INSERT INTO transfer_usage(job_id,rule_id,quota_group,ts,bytes) VALUES(?,?,?,?,?) ON CONFLICT(job_id,ts) DO UPDATE SET bytes=transfer_usage.bytes+excluded.bytes`, jobID, ruleID, group, at, delta)
	return err
}

// CompleteTask settles the immutable manifest, source index, quota and job
// together. done means the destination was verified (and move removed source).
func (s *Store) CompleteTask(ctx context.Context, jobID, status string, bytes int64, speed float64, message string, done []string, result any) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recordUsage(ctx, tx, jobID, bytes, time.Now().UnixMilli()); err != nil {
		return err
	}
	remaining := "failed"
	if status == TransferStatusDone {
		remaining = "done"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE transfer_job_files SET state=?,last_error=? WHERE job_id=?`, remaining, message, jobID); err != nil {
		return err
	}
	for _, p := range done {
		if _, err := tx.ExecContext(ctx, `UPDATE transfer_job_files SET state='done',last_error='' WHERE job_id=? AND path=?`, jobID, p); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET
 state=CASE WHEN EXISTS(SELECT 1 FROM transfer_job_files tf WHERE tf.job_id=? AND COALESCE(NULLIF(tf.source_path,''),tf.path)=files.path AND tf.state='done') THEN 'done' ELSE 'failed' END,
 last_error=?,error_ignored=CASE WHEN ?='terminated' THEN error_ignored ELSE 0 END,
 fail_count=fail_count+CASE WHEN ?='failed' THEN 1 ELSE 0 END,job_id=NULL WHERE job_id=?`, jobID, message, status, status, jobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET last_error='' WHERE rule_id=(SELECT rule_id FROM jobs WHERE job_id=?) AND state='done'`, jobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status=?,ended_at=?,bytes_done=MAX(bytes_done,?),avg_speed=?,error=?,error_ignored=0,result_snapshot=?,reserved_bytes=0,block_reason='',updated_at=? WHERE job_id=? AND status IN ('running','pending','blocked')`, status, nowUnix(), bytes, speed, message, string(encoded), nowUnix(), jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AllTaskFiles(ctx context.Context, jobID string) ([]TransferJobFile, error) {
	return s.ListTransferJobFiles(ctx, jobID, "", 1000000)
}

func (s *Store) AvailableBudget(ctx context.Context, rule Rule) (int64, error) {
	limit := rule.DailyLimitBytes
	var usage int64
	var err error
	if rule.LimitGroup != "" {
		g, ok, e := s.GetLimitGroup(ctx, rule.LimitGroup)
		if e != nil {
			return 0, e
		}
		if !ok {
			return 0, errors.New("限流分组不存在")
		}
		limit = g.DailyLimitBytes
		usage, err = s.GroupBudgetSince(ctx, rule.LimitGroup, time.Now().Add(-24*time.Hour))
	} else {
		usage, err = s.RuleBudgetSince(ctx, rule.ID, time.Now().Add(-24*time.Hour))
	}
	if err != nil {
		return 0, err
	}
	if limit <= 0 {
		return -1, nil
	}
	return max(int64(0), limit-usage), nil
}
