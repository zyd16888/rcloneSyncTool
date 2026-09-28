package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db           *sql.DB
	runtimeMu    sync.Mutex
	runtimeCache RuntimeSettings
	runtimeUntil time.Time
}

func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Migrate(ctx context.Context) error {
	const schema = `
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS remotes (
  name TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  config_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS rules (
  id TEXT PRIMARY KEY,
  limit_group TEXT NOT NULL DEFAULT '',
  src_kind TEXT NOT NULL DEFAULT 'remote',
  src_remote TEXT NOT NULL,
  src_path TEXT NOT NULL,
  src_local_root TEXT NOT NULL DEFAULT '',
  local_watch_enabled INTEGER NOT NULL DEFAULT 1,
  dst_remote TEXT NOT NULL,
  dst_path TEXT NOT NULL,
  transfer_mode TEXT NOT NULL DEFAULT 'copy',
  rclone_extra_args TEXT NOT NULL DEFAULT '',
  resume_enabled INTEGER NOT NULL DEFAULT 0,
  partial_dir TEXT NOT NULL DEFAULT '',
  partial_suffix TEXT NOT NULL DEFAULT '',
  bwlimit TEXT NOT NULL DEFAULT '',
  daily_limit_bytes INTEGER NOT NULL DEFAULT 0,
  min_file_size_bytes INTEGER NOT NULL DEFAULT 0,
  is_manual INTEGER NOT NULL DEFAULT 0,
  api_enabled INTEGER NOT NULL DEFAULT 0,
  api_allowed_operations TEXT NOT NULL DEFAULT '',
  max_parallel_jobs INTEGER NOT NULL DEFAULT 1,
  scan_interval_sec INTEGER NOT NULL DEFAULT 15,
  stable_seconds INTEGER NOT NULL DEFAULT 60,
  batch_size INTEGER NOT NULL DEFAULT 100,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS files (
  rule_id TEXT NOT NULL,
  path TEXT NOT NULL,
  size INTEGER NOT NULL,
  mod_time TEXT NOT NULL,
  state TEXT NOT NULL,
  last_seen INTEGER NOT NULL,
  seen_size INTEGER NOT NULL,
  seen_mod_time TEXT NOT NULL,
  job_id TEXT,
  fail_count INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (rule_id, path),
  FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS files_state_idx ON files(rule_id, state);
CREATE INDEX IF NOT EXISTS files_job_idx ON files(job_id);

CREATE TABLE IF NOT EXISTS jobs (
  job_id TEXT PRIMARY KEY,
  rule_id TEXT NOT NULL,
  transfer_mode TEXT NOT NULL,
  rc_port INTEGER NOT NULL,
  started_at INTEGER NOT NULL,
  ended_at INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL,
  bytes_done INTEGER NOT NULL DEFAULT 0,
  avg_speed REAL NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  log_path TEXT NOT NULL DEFAULT '',
  FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS jobs_rule_idx ON jobs(rule_id, status);

CREATE TABLE IF NOT EXISTS transfer_job_files (
  job_id TEXT NOT NULL,
  path TEXT NOT NULL,
  size INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'pending',
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (job_id, path),
  FOREIGN KEY (job_id) REFERENCES jobs(job_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS job_metrics (
  job_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  bytes INTEGER NOT NULL,
  speed REAL NOT NULL,
  transfers INTEGER NOT NULL,
  errors INTEGER NOT NULL,
  PRIMARY KEY (job_id, ts),
  FOREIGN KEY (job_id) REFERENCES jobs(job_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS limit_groups (
  name TEXT PRIMARY KEY,
  daily_limit_bytes INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS extension_presets (
  name TEXT PRIMARY KEY,
  extensions TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}

	// Incremental migrations for existing DBs.
	if err := s.ensureRuleColumn(ctx, "src_kind", "TEXT NOT NULL DEFAULT 'remote'"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "src_local_root", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "local_watch_enabled", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "rclone_extra_args", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "resume_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "partial_dir", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "partial_suffix", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "bwlimit", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "daily_limit_bytes", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "min_file_size_bytes", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "limit_group", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "is_manual", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "ignore_extensions", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "api_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureRuleColumn(ctx, "api_allowed_operations", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	// API-created transfer jobs share the jobs table so limit groups, quota
	// accounting and the job list keep working for them unchanged.
	for _, col := range [][2]string{
		{"origin", "TEXT NOT NULL DEFAULT 'scheduler'"},
		{"external_id", "TEXT NOT NULL DEFAULT ''"},
		{"idempotency_key", "TEXT NOT NULL DEFAULT ''"},
		{"request_fingerprint", "TEXT NOT NULL DEFAULT ''"},
		{"request_snapshot", "TEXT NOT NULL DEFAULT ''"},
		{"result_snapshot", "TEXT NOT NULL DEFAULT ''"},
		{"callback_url", "TEXT NOT NULL DEFAULT ''"},
		{"callback_state", "TEXT NOT NULL DEFAULT ''"},
		{"callback_attempts", "INTEGER NOT NULL DEFAULT 0"},
		{"callback_next_at", "INTEGER NOT NULL DEFAULT 0"},
		{"block_reason", "TEXT NOT NULL DEFAULT ''"},
		{"created_at", "INTEGER NOT NULL DEFAULT 0"},
		{"updated_at", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := s.ensureJobColumn(ctx, col[0], col[1]); err != nil {
			return err
		}
	}

	// Partial unique indexes: scheduler and manual jobs leave both keys empty,
	// so only API jobs are constrained. This is what makes a replayed submit
	// fail at the database instead of starting a second transfer.
	for _, ddl := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS jobs_idempotency_key_idx
		   ON jobs(idempotency_key) WHERE idempotency_key <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS jobs_external_id_idx
		   ON jobs(external_id) WHERE external_id <> ''`,
		`CREATE INDEX IF NOT EXISTS jobs_origin_status_idx ON jobs(origin, status)`,
	} {
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return s.migrateTaskLifecycle(ctx)
}

func nowUnix() int64 { return time.Now().Unix() }

func (s *Store) ensureJobColumn(ctx context.Context, col, ddl string) error {
	return s.ensureColumn(ctx, "jobs", col, ddl)
}

func (s *Store) ensureRuleColumn(ctx context.Context, col, ddl string) error {
	return s.ensureColumn(ctx, "rules", col, ddl)
}

// ensureColumn adds a column when an existing database predates it. The table
// name is a compile-time constant from this package, never caller input.
func (s *Store) ensureColumn(ctx context.Context, table, col, ddl string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+col+` `+ddl)
	return err
}

type DefaultSettings struct {
	RcloneConfigPath string
	LogDir           string
	LogRetentionDays int
	RcPortStart      int
	RcPortEnd        int
	GlobalMaxJobs    int
	Transfers        int
	Checkers         int
	BufferSize       string
	DriveChunkSize   string
	Bwlimit          string
	MetricsInterval  time.Duration
	SchedulerTick    time.Duration
}

func (s *Store) EnsureDefaultSettings(ctx context.Context, d DefaultSettings) error {
	setIfMissing := func(key, val string) error {
		_, err := s.db.ExecContext(ctx, `
INSERT INTO settings(key, value, updated_at)
VALUES(?, ?, ?)
ON CONFLICT(key) DO NOTHING
`, key, val, nowUnix())
		return err
	}

	if err := setIfMissing("rclone_config_path", d.RcloneConfigPath); err != nil {
		return err
	}
	if err := setIfMissing("log_dir", d.LogDir); err != nil {
		return err
	}
	if err := setIfMissing("log_retention_days", fmt.Sprintf("%d", d.LogRetentionDays)); err != nil {
		return err
	}
	if err := setIfMissing("rc_port_start", fmt.Sprintf("%d", d.RcPortStart)); err != nil {
		return err
	}
	if err := setIfMissing("rc_port_end", fmt.Sprintf("%d", d.RcPortEnd)); err != nil {
		return err
	}
	if err := setIfMissing("global_max_jobs", fmt.Sprintf("%d", d.GlobalMaxJobs)); err != nil {
		return err
	}
	if err := setIfMissing("rclone_transfers", fmt.Sprintf("%d", d.Transfers)); err != nil {
		return err
	}
	if err := setIfMissing("rclone_checkers", fmt.Sprintf("%d", d.Checkers)); err != nil {
		return err
	}
	if err := setIfMissing("rclone_buffer_size", d.BufferSize); err != nil {
		return err
	}
	if err := setIfMissing("rclone_drive_chunk_size", d.DriveChunkSize); err != nil {
		return err
	}
	if err := setIfMissing("rclone_bwlimit", d.Bwlimit); err != nil {
		return err
	}
	if err := setIfMissing("metrics_interval_ms", fmt.Sprintf("%d", d.MetricsInterval.Milliseconds())); err != nil {
		return err
	}
	if err := setIfMissing("scheduler_tick_ms", fmt.Sprintf("%d", d.SchedulerTick.Milliseconds())); err != nil {
		return err
	}
	return nil
}

func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var val string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}
