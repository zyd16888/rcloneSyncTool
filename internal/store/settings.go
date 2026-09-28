package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

type SettingKV struct {
	Key       string
	Value     string
	UpdatedAt time.Time
}

func (s *Store) ListSettings(ctx context.Context) ([]SettingKV, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value, updated_at FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettingKV
	for rows.Next() {
		var kv SettingKV
		var updated int64
		if err := rows.Scan(&kv.Key, &kv.Value, &updated); err != nil {
			return nil, err
		}
		kv.UpdatedAt = time.Unix(updated, 0)
		out = append(out, kv)
	}
	return out, rows.Err()
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	defer s.invalidateRuntimeSettings()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO settings(key, value, updated_at)
VALUES(?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
  value=excluded.value,
  updated_at=excluded.updated_at
`, key, value, nowUnix())
	return err
}

type RuntimeSettings struct {
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
	ScanTimeout      time.Duration
}

func (s *Store) RuntimeSettings(ctx context.Context) (RuntimeSettings, error) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if time.Now().Before(s.runtimeUntil) {
		return s.runtimeCache, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM settings WHERE key IN ('rclone_config_path','log_dir','log_retention_days','rc_port_start','rc_port_end','global_max_jobs','rclone_transfers','rclone_checkers','rclone_buffer_size','rclone_drive_chunk_size','rclone_bwlimit','metrics_interval_ms','scheduler_tick_ms','scan_timeout_sec')`)
	if err != nil {
		return RuntimeSettings{}, err
	}
	m := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return RuntimeSettings{}, err
		}
		m[key] = value
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return RuntimeSettings{}, err
	}
	rows.Close()
	rs := RuntimeSettings{
		RcloneConfigPath: m["rclone_config_path"],
		LogDir:           m["log_dir"],
		LogRetentionDays: parseIntDefault(m["log_retention_days"], 7),
		RcPortStart:      parseIntDefault(m["rc_port_start"], 55720),
		RcPortEnd:        parseIntDefault(m["rc_port_end"], 55800),
		GlobalMaxJobs:    parseIntDefault(m["global_max_jobs"], 0),
		Transfers:        parseIntDefault(m["rclone_transfers"], 4),
		Checkers:         parseIntDefault(m["rclone_checkers"], 8),
		BufferSize:       m["rclone_buffer_size"],
		DriveChunkSize:   m["rclone_drive_chunk_size"],
		Bwlimit:          m["rclone_bwlimit"],
		MetricsInterval:  time.Duration(parseIntDefault(m["metrics_interval_ms"], 2000)) * time.Millisecond,
		SchedulerTick:    time.Duration(parseIntDefault(m["scheduler_tick_ms"], 2000)) * time.Millisecond,
		ScanTimeout:      time.Duration(parseIntDefault(m["scan_timeout_sec"], 600)) * time.Second,
	}
	if rs.GlobalMaxJobs < 0 || rs.RcPortStart < 1 || rs.RcPortEnd > 65535 || rs.RcPortEnd < rs.RcPortStart || rs.Transfers < 1 || rs.Checkers < 1 || rs.MetricsInterval <= 0 || rs.SchedulerTick <= 0 || rs.ScanTimeout <= 0 || rs.LogRetentionDays < 0 {
		return RuntimeSettings{}, fmt.Errorf("系统设置无效：并发、端口及采样/调度/扫描间隔必须在有效范围内")
	}
	s.runtimeCache = rs
	s.runtimeUntil = time.Now().Add(time.Second)
	return rs, nil
}

func (s *Store) invalidateRuntimeSettings() {
	s.runtimeMu.Lock()
	s.runtimeUntil = time.Time{}
	s.runtimeMu.Unlock()
}

func (s *Store) SetSettings(ctx context.Context, values map[string]string) error {
	defer s.invalidateRuntimeSettings()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, value, nowUnix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	defer s.invalidateRuntimeSettings()
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, key)
	return err
}

func (s *Store) Keys(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, rows.Err()
}

func (s *Store) MustSetting(ctx context.Context, key string) (string, error) {
	var val string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("missing setting: " + key)
	}
	return val, err
}
