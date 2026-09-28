package store

import (
	"context"
	"time"
)

type RuleRuntime struct {
	ScanStartedAt time.Time
	ScanEndedAt   time.Time
	ScanError     string
	Discovered    int
	Eligible      int
	Filtered      int
	Enqueued      int64
	BlockReason   string
	BlockMessage  string
}

func (s *Store) StartRuleScan(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO rule_runtime(rule_id,scan_started_at) VALUES(?,?) ON CONFLICT(rule_id) DO UPDATE SET scan_started_at=excluded.scan_started_at,scan_error=''`, id, nowUnix())
	return err
}
func (s *Store) FinishRuleScan(ctx context.Context, id string, stats ScanStats, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rule_runtime SET scan_ended_at=?,scan_error=?,discovered=?,eligible=?,filtered=?,enqueued=? WHERE rule_id=?`, nowUnix(), message, stats.Discovered, stats.Eligible, stats.Filtered, stats.Enqueued, id)
	return err
}
func (s *Store) SetRuleBlock(ctx context.Context, id, reason, message string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO rule_runtime(rule_id,block_reason,block_message) VALUES(?,?,?) ON CONFLICT(rule_id) DO UPDATE SET block_reason=excluded.block_reason,block_message=excluded.block_message`, id, reason, message)
	return err
}
func (s *Store) RuleRuntimes(ctx context.Context) (map[string]RuleRuntime, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rule_id,scan_started_at,scan_ended_at,scan_error,discovered,eligible,filtered,enqueued,block_reason,block_message FROM rule_runtime`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RuleRuntime{}
	for rows.Next() {
		var id string
		var r RuleRuntime
		var started, ended int64
		if err := rows.Scan(&id, &started, &ended, &r.ScanError, &r.Discovered, &r.Eligible, &r.Filtered, &r.Enqueued, &r.BlockReason, &r.BlockMessage); err != nil {
			return nil, err
		}
		if started > 0 {
			r.ScanStartedAt = time.Unix(started, 0)
		}
		if ended > 0 {
			r.ScanEndedAt = time.Unix(ended, 0)
		}
		out[id] = r
	}
	return out, rows.Err()
}

type RuleActivity struct{ Running, Pending, Failed int }

func (s *Store) RuleActivities(ctx context.Context) (map[string]RuleActivity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rule_id,status,COUNT(*) FROM jobs WHERE status IN ('running','pending','blocked','failed') GROUP BY rule_id,status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RuleActivity{}
	for rows.Next() {
		var id, status string
		var n int
		if err := rows.Scan(&id, &status, &n); err != nil {
			return nil, err
		}
		a := out[id]
		switch status {
		case "running":
			a.Running = n
		case "pending", "blocked":
			a.Pending += n
		case "failed":
			a.Failed = n
		}
		out[id] = a
	}
	return out, rows.Err()
}
func (s *Store) RuleCountsAll(ctx context.Context) (map[string]FileStateCounts, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rule_id,state,COUNT(*) FROM files GROUP BY rule_id,state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FileStateCounts{}
	for rows.Next() {
		var id, state string
		var n int
		if err := rows.Scan(&id, &state, &n); err != nil {
			return nil, err
		}
		c := out[id]
		switch state {
		case "new":
			c.New = n
		case "stable":
			c.Stable = n
		case "queued":
			c.Queued = n
		case "transferring":
			c.Transferring = n
		case "done":
			c.Done = n
		case "failed":
			c.Failed = n
		case "missing":
			c.Missing = n
		}
		out[id] = c
	}
	return out, rows.Err()
}
