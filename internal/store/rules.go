package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const ruleColumns = `id, limit_group, src_kind, src_remote, src_path, src_local_root, local_watch_enabled,
 dst_remote, dst_path, transfer_mode, rclone_extra_args, resume_enabled, partial_dir, partial_suffix,
 ignore_extensions, bwlimit, daily_limit_bytes, min_file_size_bytes, is_manual, api_enabled, api_allowed_operations,
 max_parallel_jobs, scan_interval_sec, stable_seconds, batch_size, enabled,
 group_by_directory, atomic_publish, staging_path, ready_marker, created_at, updated_at`

func scanRuleRow(scan func(...any) error) (Rule, error) {
	var r Rule
	var watch, resume, manual, api, enabled, grouped, atomic int
	var created, updated int64
	err := scan(&r.ID, &r.LimitGroup, &r.SrcKind, &r.SrcRemote, &r.SrcPath, &r.SrcLocalRoot, &watch,
		&r.DstRemote, &r.DstPath, &r.TransferMode, &r.RcloneExtraArgs, &resume, &r.PartialDir, &r.PartialSuffix,
		&r.IgnoreExtensions, &r.Bwlimit, &r.DailyLimitBytes, &r.MinFileSizeBytes, &manual, &api, &r.APIAllowedOperations,
		&r.MaxParallelJobs, &r.ScanIntervalSec, &r.StableSeconds, &r.BatchSize, &enabled,
		&grouped, &atomic, &r.StagingPath, &r.ReadyMarker, &created, &updated)
	r.LocalWatch, r.ResumeEnabled, r.IsManual, r.APIEnabled, r.Enabled = watch != 0, resume != 0, manual != 0, api != 0, enabled != 0
	r.GroupByDirectory, r.AtomicPublish = grouped != 0, atomic != 0
	r.CreatedAt, r.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return r, err
}

func (s *Store) ListRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ruleColumns+` FROM rules WHERE is_manual=0 AND is_deleted=0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRuleRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetRule(ctx context.Context, id string) (Rule, bool, error) {
	r, err := scanRuleRow(s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM rules WHERE id=? AND is_deleted=0`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, false, nil
	}
	return r, err == nil, err
}

func ruleTransferChanged(a, b Rule) bool {
	return a.SrcKind != b.SrcKind || a.SrcRemote != b.SrcRemote || a.SrcPath != b.SrcPath || a.SrcLocalRoot != b.SrcLocalRoot ||
		a.DstRemote != b.DstRemote || a.DstPath != b.DstPath || a.TransferMode != b.TransferMode ||
		a.GroupByDirectory != b.GroupByDirectory || a.AtomicPublish != b.AtomicPublish || a.StagingPath != b.StagingPath || a.ReadyMarker != b.ReadyMarker
}

func (s *Store) UpsertRule(ctx context.Context, r Rule) error {
	if err := r.Normalize(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var deleted int
	if err := tx.QueryRowContext(ctx, `SELECT is_deleted FROM rules WHERE id=?`, r.ID).Scan(&deleted); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if deleted != 0 {
		return errors.New("该规则 ID 已用于历史任务，请使用新的 ID")
	}
	old, oldErr := scanRuleRow(tx.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM rules WHERE id=?`, r.ID).Scan)
	if oldErr != nil && !errors.Is(oldErr, sql.ErrNoRows) {
		return oldErr
	}
	if oldErr == nil && ruleTransferChanged(old, r) {
		var running int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE rule_id=? AND status='running'`, r.ID).Scan(&running); err != nil {
			return err
		}
		if running > 0 {
			return errors.New("规则存在运行任务，请完成或终止任务后再修改传输路径、模式或分组方式")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='terminated', ended_at=?, updated_at=?, error='rule transfer configuration changed', reserved_bytes=0 WHERE rule_id=? AND status IN ('pending','blocked')`, nowUnix(), nowUnix(), r.ID); err != nil {
			return err
		}
		// This only invalidates the local index. Source and destination files are
		// reconciled by the new execution plan; no media is deleted here.
		if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE rule_id=?`, r.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM file_groups WHERE rule_id=?`, r.ID); err != nil {
			return err
		}
	}
	now := nowUnix()
	args := []any{r.ID, r.LimitGroup, r.SrcKind, r.SrcRemote, r.SrcPath, r.SrcLocalRoot, boolToInt(r.LocalWatch),
		r.DstRemote, r.DstPath, r.TransferMode, r.RcloneExtraArgs, boolToInt(r.ResumeEnabled), r.PartialDir, r.PartialSuffix,
		r.IgnoreExtensions, r.Bwlimit, r.DailyLimitBytes, r.MinFileSizeBytes, boolToInt(r.IsManual), boolToInt(r.APIEnabled), r.APIAllowedOperations,
		r.MaxParallelJobs, r.ScanIntervalSec, r.StableSeconds, r.BatchSize, boolToInt(r.Enabled),
		boolToInt(r.GroupByDirectory), boolToInt(r.AtomicPublish), r.StagingPath, r.ReadyMarker, now, now}
	var updates []string
	for _, col := range strings.Split(ruleColumns, ",") {
		col = strings.TrimSpace(col)
		if col != "id" && col != "created_at" {
			updates = append(updates, col+"=excluded."+col)
		}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	if _, err := tx.ExecContext(ctx, `INSERT INTO rules(`+ruleColumns+`) VALUES(`+placeholders+`) ON CONFLICT(id) DO UPDATE SET `+strings.Join(updates, ","), args...); err != nil {
		return err
	}
	return tx.Commit()
}

// Keep task history and quota accounting when a rule is removed from the UI.
func (s *Store) DeleteRule(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE rule_id=? AND status='running'`, id).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return errors.New("规则仍有运行任务，请完成或终止任务后删除")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rules SET is_deleted=1, enabled=0, api_enabled=0, updated_at=? WHERE id=?`, nowUnix(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='terminated', ended_at=?, updated_at=?, reserved_bytes=0, error='rule deleted' WHERE rule_id=? AND status IN ('pending','blocked')`, nowUnix(), nowUnix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetRulesByGroup(ctx context.Context, group string) ([]Rule, error) {
	rules, err := s.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	var out []Rule
	for _, rule := range rules {
		if rule.LimitGroup == group {
			out = append(out, rule)
		}
	}
	return out, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
