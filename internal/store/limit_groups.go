package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *Store) ListLimitGroups(ctx context.Context) ([]LimitGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, daily_limit_bytes, updated_at FROM limit_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LimitGroup
	for rows.Next() {
		var g LimitGroup
		var updated int64
		if err := rows.Scan(&g.Name, &g.DailyLimitBytes, &updated); err != nil {
			return nil, err
		}
		g.UpdatedAt = time.Unix(updated, 0)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) GetLimitGroup(ctx context.Context, name string) (LimitGroup, bool, error) {
	var g LimitGroup
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT name, daily_limit_bytes, updated_at FROM limit_groups WHERE name=?`, name).Scan(&g.Name, &g.DailyLimitBytes, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return LimitGroup{}, false, nil
	}
	if err != nil {
		return LimitGroup{}, false, err
	}
	g.UpdatedAt = time.Unix(updated, 0)
	return g, true, nil
}

func (s *Store) UpsertLimitGroup(ctx context.Context, g LimitGroup) error {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" {
		return errors.New("group name required")
	}
	if g.DailyLimitBytes < 0 {
		g.DailyLimitBytes = 0
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO limit_groups(name, daily_limit_bytes, updated_at)
VALUES(?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  daily_limit_bytes=excluded.daily_limit_bytes,
  updated_at=excluded.updated_at
`, g.Name, g.DailyLimitBytes, nowUnix())
	return err
}

func (s *Store) DeleteLimitGroup(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM limit_groups WHERE name=?`, name)
	return err
}
