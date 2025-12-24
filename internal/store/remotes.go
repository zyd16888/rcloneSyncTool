package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Store) ListRemotes(ctx context.Context) ([]Remote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, type, config_json, updated_at FROM remotes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Remote
	for rows.Next() {
		var r Remote
		var updated int64
		if err := rows.Scan(&r.Name, &r.Type, &r.ConfigJSON, &updated); err != nil {
			return nil, err
		}
		r.UpdatedAt = time.Unix(updated, 0)
		_ = r.UnmarshalConfig()
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetRemote(ctx context.Context, name string) (Remote, bool, error) {
	var r Remote
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT name, type, config_json, updated_at FROM remotes WHERE name=?`, name).
		Scan(&r.Name, &r.Type, &r.ConfigJSON, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Remote{}, false, nil
	}
	if err != nil {
		return Remote{}, false, err
	}
	r.UpdatedAt = time.Unix(updated, 0)
	_ = r.UnmarshalConfig()
	return r, true, nil
}

func (s *Store) UpsertRemote(ctx context.Context, r Remote) error {
	if err := r.MarshalConfig(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO remotes(name, type, config_json, updated_at)
VALUES(?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  type=excluded.type,
  config_json=excluded.config_json,
  updated_at=excluded.updated_at
`, r.Name, r.Type, r.ConfigJSON, nowUnix())
	return err
}

func (s *Store) DeleteRemote(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM remotes WHERE name=?`, name)
	return err
}

