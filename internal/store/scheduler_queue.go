package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

type QueuedGroup struct {
	Key, Directory, Signature string
	Files                     []TransferJobFile
	Bytes                     int64
}

func (s *Store) QueuedGroups(ctx context.Context, rule Rule) ([]QueuedGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.group_key,g.directory,g.signature,f.path,f.size,f.mod_time
FROM file_groups g JOIN files f ON f.rule_id=g.rule_id AND f.group_key=g.group_key
WHERE g.rule_id=? AND g.ready=1 AND g.changed_at<=? AND f.source_present=1
AND EXISTS(SELECT 1 FROM files q WHERE q.rule_id=g.rule_id AND q.group_key=g.group_key AND q.state='queued' AND q.source_present=1 AND (q.job_id IS NULL OR q.job_id=''))
AND NOT EXISTS(SELECT 1 FROM files bad WHERE bad.rule_id=g.rule_id AND bad.group_key=g.group_key AND bad.source_present=1 AND bad.state IN ('new','failed','transferring'))
AND NOT EXISTS(SELECT 1 FROM jobs j WHERE j.rule_id=g.rule_id AND j.status IN ('pending','blocked','running') AND (j.group_key=g.group_key OR EXISTS(SELECT 1 FROM transfer_job_files tf WHERE tf.job_id=j.job_id AND tf.group_key=g.group_key)))
ORDER BY g.changed_at,g.group_key,f.path`, rule.ID, time.Now().UnixMilli()-int64(rule.StableSeconds)*1000)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedGroup
	for rows.Next() {
		var key, dir, signature string
		var f TransferJobFile
		if err := rows.Scan(&key, &dir, &signature, &f.Path, &f.Size, &f.ModTime); err != nil {
			return nil, err
		}
		f.SourcePath, f.GroupKey = f.Path, key
		if len(out) == 0 || out[len(out)-1].Key != key {
			out = append(out, QueuedGroup{Key: key, Directory: dir, Signature: signature})
		}
		g := &out[len(out)-1]
		g.Files = append(g.Files, f)
		g.Bytes += f.Size
	}
	return out, rows.Err()
}

// QueueSchedulerTask binds complete groups and the frozen manifest in one
// transaction. A concurrently queued API task or a changed scan cannot split it.
func (s *Store) QueueSchedulerTask(ctx context.Context, job TransferJob, groups []QueuedGroup, files []TransferJobFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled, deleted int
	if err := tx.QueryRowContext(ctx, `SELECT enabled,is_deleted FROM rules WHERE id=?`, job.RuleID).Scan(&enabled, &deleted); err != nil {
		return err
	}
	if enabled == 0 || deleted != 0 {
		return errors.New("规则已暂停或删除")
	}
	for _, g := range groups {
		var sig string
		if err := tx.QueryRowContext(ctx, `SELECT signature FROM file_groups WHERE rule_id=? AND group_key=?`, job.RuleID, g.Key).Scan(&sig); err != nil {
			return err
		}
		if sig != g.Signature {
			return errors.New("影片目录在排队前发生变化")
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs j WHERE j.rule_id=? AND j.status IN ('pending','blocked','running') AND (j.group_key=? OR EXISTS(SELECT 1 FROM transfer_job_files tf WHERE tf.job_id=j.job_id AND tf.group_key=?))`, job.RuleID, g.Key, g.Key).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return ErrTaskNotWaiting
		}
	}
	now := nowUnix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(job_id,rule_id,origin,transfer_mode,rc_port,started_at,status,log_path,request_snapshot,quota_group,group_key,phase,created_at,updated_at,prepared) VALUES(?,?,'scheduler',?,0,0,'pending',?,?,?,?,'copying',?,?,1)`, job.JobID, job.RuleID, job.TransferMode, job.LogPath, job.RequestJSON, job.QuotaGroup, job.GroupKey, now, now); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO transfer_job_files(job_id,path,size,state,last_error,mod_time,group_key,source_path) VALUES(?,?,?,'pending','',?,?,?)`, job.JobID, f.Path, f.Size, f.ModTime, f.GroupKey, f.SourcePath); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET state='queued',job_id=? WHERE rule_id=? AND path=? AND source_present=1`, job.JobID, job.RuleID, f.SourcePath); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func RelativeGroupFiles(g QueuedGroup) []TransferJobFile {
	files := make([]TransferJobFile, 0, len(g.Files))
	for _, f := range g.Files {
		if g.Directory != "" {
			f.Path = strings.TrimPrefix(f.Path, g.Directory+"/")
		}
		files = append(files, f)
	}
	return files
}
