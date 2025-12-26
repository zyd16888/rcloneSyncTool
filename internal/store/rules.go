package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Store) ListRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, limit_group, src_kind, src_remote, src_path, src_local_root, local_watch_enabled,
       dst_remote, dst_path, transfer_mode, rclone_extra_args, ignore_extensions, bwlimit,
       daily_limit_bytes, min_file_size_bytes, is_manual,
       max_parallel_jobs, scan_interval_sec, stable_seconds, batch_size, enabled,
       created_at, updated_at
FROM rules
WHERE is_manual=0
ORDER BY id
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		var enabled int
		var watch int
		var isManual int
		var created, updated int64
		if err := rows.Scan(
			&r.ID, &r.LimitGroup, &r.SrcKind, &r.SrcRemote, &r.SrcPath, &r.SrcLocalRoot, &watch,
			&r.DstRemote, &r.DstPath, &r.TransferMode, &r.RcloneExtraArgs, &r.IgnoreExtensions, &r.Bwlimit,
			&r.DailyLimitBytes, &r.MinFileSizeBytes, &isManual,
			&r.MaxParallelJobs, &r.ScanIntervalSec, &r.StableSeconds, &r.BatchSize, &enabled,
			&created, &updated,
		); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		r.LocalWatch = watch != 0
		r.IsManual = isManual != 0
		r.CreatedAt = time.Unix(created, 0)
		r.UpdatedAt = time.Unix(updated, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetRule(ctx context.Context, id string) (Rule, bool, error) {
	var r Rule
	var enabled int
	var watch int
	var isManual int
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `
SELECT id, limit_group, src_kind, src_remote, src_path, src_local_root, local_watch_enabled,
       dst_remote, dst_path, transfer_mode, rclone_extra_args, ignore_extensions, bwlimit,
       daily_limit_bytes, min_file_size_bytes, is_manual,
       max_parallel_jobs, scan_interval_sec, stable_seconds, batch_size, enabled,
       created_at, updated_at
FROM rules
WHERE id=?
`, id).Scan(
		&r.ID, &r.LimitGroup, &r.SrcKind, &r.SrcRemote, &r.SrcPath, &r.SrcLocalRoot, &watch,
		&r.DstRemote, &r.DstPath, &r.TransferMode, &r.RcloneExtraArgs, &r.IgnoreExtensions, &r.Bwlimit,
		&r.DailyLimitBytes, &r.MinFileSizeBytes, &isManual,
		&r.MaxParallelJobs, &r.ScanIntervalSec, &r.StableSeconds, &r.BatchSize, &enabled,
		&created, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, false, nil
	}
	if err != nil {
		return Rule{}, false, err
	}
	r.Enabled = enabled != 0
	r.LocalWatch = watch != 0
	r.IsManual = isManual != 0
	r.CreatedAt = time.Unix(created, 0)
	r.UpdatedAt = time.Unix(updated, 0)
	return r, true, nil
}

func (s *Store) UpsertRule(ctx context.Context, r Rule) error {
	if err := r.Normalize(); err != nil {
		return err
	}
	now := nowUnix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO rules(
  id, limit_group, src_kind, src_remote, src_path, src_local_root, local_watch_enabled,
  dst_remote, dst_path, transfer_mode, rclone_extra_args, ignore_extensions, bwlimit,
  daily_limit_bytes, min_file_size_bytes, is_manual,
  max_parallel_jobs, scan_interval_sec, stable_seconds, batch_size, enabled,
  created_at, updated_at
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  limit_group=excluded.limit_group,
  src_kind=excluded.src_kind,
  src_remote=excluded.src_remote,
  src_path=excluded.src_path,
  src_local_root=excluded.src_local_root,
  local_watch_enabled=excluded.local_watch_enabled,
  dst_remote=excluded.dst_remote,
  dst_path=excluded.dst_path,
  transfer_mode=excluded.transfer_mode,
  rclone_extra_args=excluded.rclone_extra_args,
  ignore_extensions=excluded.ignore_extensions,
  bwlimit=excluded.bwlimit,
  daily_limit_bytes=excluded.daily_limit_bytes,
  min_file_size_bytes=excluded.min_file_size_bytes,
  is_manual=excluded.is_manual,
  max_parallel_jobs=excluded.max_parallel_jobs,
  scan_interval_sec=excluded.scan_interval_sec,
  stable_seconds=excluded.stable_seconds,
  batch_size=excluded.batch_size,
  enabled=excluded.enabled,
  updated_at=excluded.updated_at
`, r.ID, r.LimitGroup, r.SrcKind, r.SrcRemote, r.SrcPath, r.SrcLocalRoot, boolToInt(r.LocalWatch),
		r.DstRemote, r.DstPath, r.TransferMode, r.RcloneExtraArgs, r.IgnoreExtensions, r.Bwlimit,
		r.DailyLimitBytes, r.MinFileSizeBytes, boolToInt(r.IsManual),
		r.MaxParallelJobs, r.ScanIntervalSec, r.StableSeconds, r.BatchSize, boolToInt(r.Enabled),
		now, now,
	)
	return err
}

func (s *Store) DeleteRule(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM rules WHERE id=?`, id)
	return err
}

func (s *Store) GetRulesByGroup(ctx context.Context, group string) ([]Rule, error) {
	if group == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, limit_group, src_kind, src_remote, src_path, src_local_root, local_watch_enabled,
       dst_remote, dst_path, transfer_mode, rclone_extra_args, ignore_extensions, bwlimit,
       daily_limit_bytes, min_file_size_bytes, is_manual,
       max_parallel_jobs, scan_interval_sec, stable_seconds, batch_size, enabled,
       created_at, updated_at
FROM rules
WHERE limit_group=? AND is_manual=0
`, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		var enabled int
		var watch int
		var isManual int
		var created, updated int64
		if err := rows.Scan(
			&r.ID, &r.LimitGroup, &r.SrcKind, &r.SrcRemote, &r.SrcPath, &r.SrcLocalRoot, &watch,
			&r.DstRemote, &r.DstPath, &r.TransferMode, &r.RcloneExtraArgs, &r.IgnoreExtensions, &r.Bwlimit,
			&r.DailyLimitBytes, &r.MinFileSizeBytes, &isManual,
			&r.MaxParallelJobs, &r.ScanIntervalSec, &r.StableSeconds, &r.BatchSize, &enabled,
			&created, &updated,
		); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		r.LocalWatch = watch != 0
		r.IsManual = isManual != 0
		r.CreatedAt = time.Unix(created, 0)
		r.UpdatedAt = time.Unix(updated, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
