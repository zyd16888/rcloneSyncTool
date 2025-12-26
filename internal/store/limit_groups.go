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

func (s *Store) SetRulesForLimitGroup(ctx context.Context, groupName string, ruleIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Remove this group from all rules that currently have it (reset to empty)
	if _, err := tx.ExecContext(ctx, `UPDATE rules SET limit_group='' WHERE limit_group=?`, groupName); err != nil {
		return err
	}

	// 2. Set this group for the provided rule IDs
	if len(ruleIDs) > 0 {
		// Prepare a query with placeholders
		query := `UPDATE rules SET limit_group=? WHERE id IN (?` + strings.Repeat(",?", len(ruleIDs)-1) + `)`
		args := make([]any, len(ruleIDs)+1)
		args[0] = groupName
		for i, id := range ruleIDs {
			args[i+1] = id
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}

	return tx.Commit()
}
