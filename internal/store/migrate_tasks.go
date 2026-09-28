package store

import "context"

func (s *Store) migrateTaskLifecycle(ctx context.Context) error {
	columns := map[string][][2]string{
		"rules": {
			{"group_by_directory", "INTEGER NOT NULL DEFAULT 0"},
			{"atomic_publish", "INTEGER NOT NULL DEFAULT 0"},
			{"staging_path", "TEXT NOT NULL DEFAULT ''"},
			{"ready_marker", "TEXT NOT NULL DEFAULT ''"},
			{"is_deleted", "INTEGER NOT NULL DEFAULT 0"},
		},
		"files": {
			{"group_key", "TEXT NOT NULL DEFAULT ''"},
			{"source_present", "INTEGER NOT NULL DEFAULT 1"},
			{"error_ignored", "INTEGER NOT NULL DEFAULT 0"},
		},
		"jobs": {
			{"reserved_bytes", "INTEGER NOT NULL DEFAULT 0"},
			{"quota_group", "TEXT DEFAULT NULL"},
			{"phase", "TEXT NOT NULL DEFAULT ''"},
			{"group_key", "TEXT NOT NULL DEFAULT ''"},
			{"stage_path", "TEXT NOT NULL DEFAULT ''"},
			{"retry_of", "TEXT NOT NULL DEFAULT ''"},
			{"queue_next_at", "INTEGER NOT NULL DEFAULT 0"},
			{"prepared", "INTEGER NOT NULL DEFAULT 0"},
			{"error_ignored", "INTEGER NOT NULL DEFAULT 0"},
		},
		"transfer_job_files": {
			{"mod_time", "TEXT NOT NULL DEFAULT ''"},
			{"group_key", "TEXT NOT NULL DEFAULT ''"},
			{"source_path", "TEXT NOT NULL DEFAULT ''"},
		},
	}
	for table, cols := range columns {
		for _, col := range cols {
			if err := s.ensureColumn(ctx, table, col[0], col[1]); err != nil {
				return err
			}
		}
	}
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS file_groups (
 rule_id TEXT NOT NULL, group_key TEXT NOT NULL, directory TEXT NOT NULL DEFAULT '',
 signature TEXT NOT NULL, changed_at INTEGER NOT NULL, last_seen INTEGER NOT NULL,
 ready INTEGER NOT NULL DEFAULT 1,
 PRIMARY KEY(rule_id, group_key), FOREIGN KEY(rule_id) REFERENCES rules(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS rule_runtime (
 rule_id TEXT PRIMARY KEY, scan_started_at INTEGER NOT NULL DEFAULT 0,
 scan_ended_at INTEGER NOT NULL DEFAULT 0, scan_error TEXT NOT NULL DEFAULT '',
 scan_error_ignored INTEGER NOT NULL DEFAULT 0,
 discovered INTEGER NOT NULL DEFAULT 0, eligible INTEGER NOT NULL DEFAULT 0,
 filtered INTEGER NOT NULL DEFAULT 0, enqueued INTEGER NOT NULL DEFAULT 0,
 block_reason TEXT NOT NULL DEFAULT '', block_message TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(rule_id) REFERENCES rules(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS transfer_usage (
 job_id TEXT NOT NULL, rule_id TEXT NOT NULL, quota_group TEXT NOT NULL DEFAULT '',
 ts INTEGER NOT NULL, bytes INTEGER NOT NULL,
 PRIMARY KEY(job_id, ts), FOREIGN KEY(job_id) REFERENCES jobs(job_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS jobs_started_idx ON jobs(started_at DESC, job_id);
CREATE INDEX IF NOT EXISTS jobs_queue_idx ON jobs(status, queue_next_at, created_at);
CREATE INDEX IF NOT EXISTS files_group_idx ON files(rule_id, group_key, state);
CREATE INDEX IF NOT EXISTS manifest_group_idx ON transfer_job_files(group_key, job_id);
CREATE INDEX IF NOT EXISTS usage_window_idx ON transfer_usage(ts, rule_id, quota_group);
UPDATE jobs SET quota_group=COALESCE((SELECT limit_group FROM rules WHERE id=rule_id),'') WHERE quota_group IS NULL;
UPDATE jobs SET origin='manual' WHERE origin='scheduler' AND rule_id LIKE 'manual_%';
`)
	if err != nil {
		return err
	}
	return s.ensureColumn(ctx, "rule_runtime", "scan_error_ignored", "INTEGER NOT NULL DEFAULT 0")
}
