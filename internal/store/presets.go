package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Store) ListExtensionPresets(ctx context.Context) ([]ExtensionPreset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, extensions, updated_at FROM extension_presets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExtensionPreset
	for rows.Next() {
		var p ExtensionPreset
		var updated int64
		if err := rows.Scan(&p.Name, &p.Extensions, &updated); err != nil {
			return nil, err
		}
		p.UpdatedAt = time.Unix(updated, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetExtensionPreset(ctx context.Context, name string) (ExtensionPreset, bool, error) {
	var p ExtensionPreset
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT name, extensions, updated_at FROM extension_presets WHERE name=?`, name).Scan(&p.Name, &p.Extensions, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ExtensionPreset{}, false, nil
	}
	if err != nil {
		return ExtensionPreset{}, false, err
	}
	p.UpdatedAt = time.Unix(updated, 0)
	return p, true, nil
}

func (s *Store) UpsertExtensionPreset(ctx context.Context, p ExtensionPreset) error {
	now := nowUnix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO extension_presets(name, extensions, updated_at)
VALUES(?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  extensions=excluded.extensions,
  updated_at=excluded.updated_at
`, p.Name, p.Extensions, now)
	return err
}

func (s *Store) DeleteExtensionPreset(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM extension_presets WHERE name=?`, name)
	return err
}
