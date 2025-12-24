package store

import (
	"context"
	"database/sql"
	"errors"
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

func (s *Store) RuntimeSettings(ctx context.Context) (RuntimeSettings, error) {
	settings, err := s.ListSettings(ctx)
	if err != nil {
		return RuntimeSettings{}, err
	}
	m := map[string]string{}
	for _, kv := range settings {
		m[kv.Key] = kv.Value
	}
	return RuntimeSettings{
		RcloneConfigPath: m["rclone_config_path"],
		LogDir:           m["log_dir"],
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
	}, nil
}

func (s *Store) DeleteSetting(ctx context.Context, key string) error {
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
