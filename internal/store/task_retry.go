package store

import (
	"context"
	"errors"
)

func (s *Store) CreateTaskRetry(ctx context.Context, source TransferJob, retry TransferJob, files []TransferJobFile) error {
	if !source.Terminal() || source.Status == TransferStatusDone {
		return errors.New("仅失败或终止任务可以重试")
	}
	if source.RequestJSON == "" {
		return errors.New("旧任务没有持久清单，请在规则页重试失败文件")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE retry_of=? AND status IN ('pending','blocked','running')`, source.JobID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return errors.New("原任务已有正在等待或执行的重试")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM transfer_job_files tf JOIN jobs j ON j.job_id=tf.job_id WHERE j.rule_id=? AND j.status IN ('pending','blocked','running') AND tf.group_key<>'' AND tf.group_key IN (SELECT group_key FROM transfer_job_files WHERE job_id=?)`, source.RuleID, source.JobID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return errors.New("影片组已有等待或运行任务")
	}
	now := nowUnix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(job_id,rule_id,origin,transfer_mode,rc_port,started_at,status,log_path,idempotency_key,request_fingerprint,request_snapshot,callback_url,created_at,updated_at,quota_group,group_key,phase,stage_path,retry_of,prepared) VALUES(?,?,?,?,0,0,'pending',?,?,?,?,?,?,?,?,?,?,?,?,?)`, retry.JobID, source.RuleID, source.Origin, retry.TransferMode, retry.LogPath, retry.IdempotencyKey, retry.Fingerprint, retry.RequestJSON, source.CallbackURL, now, now, source.QuotaGroup, source.GroupKey, retry.Phase, source.StagePath, source.JobID, boolToInt(retry.Prepared)); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO transfer_job_files(job_id,path,size,state,last_error,mod_time,group_key,source_path) VALUES(?,?,?,'pending','',?,?,?)`, retry.JobID, f.Path, f.Size, f.ModTime, f.GroupKey, f.SourcePath); err != nil {
			return err
		}
		if source.Origin == OriginScheduler {
			if _, err := tx.ExecContext(ctx, `UPDATE files SET state='queued',job_id=?,last_error='' WHERE rule_id=? AND path=? AND (job_id IS NULL OR job_id='')`, retry.JobID, source.RuleID, f.SourcePath); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) FailedRuleTasks(ctx context.Context, ruleID string) ([]TransferJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+transferJobColumns+` FROM jobs j WHERE rule_id=? AND status IN ('failed','terminated') AND request_snapshot<>''
AND NOT EXISTS(SELECT 1 FROM jobs child WHERE child.retry_of=j.job_id)
ORDER BY created_at DESC,job_id`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TransferJob
	for rows.Next() {
		j, err := scanTransferJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
