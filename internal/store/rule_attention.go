package store

import (
	"context"
	"database/sql"
)

// A failed scheduler task is current only while its source index still has
// failed files. Stopped tasks and superseded retry ancestors remain history.
func currentRuleTaskFailureSQL() string {
	return `j.status='failed'
AND NOT EXISTS(SELECT 1 FROM jobs child WHERE child.retry_of=j.job_id)
AND (j.origin<>'scheduler' OR EXISTS(
 SELECT 1 FROM transfer_job_files tf JOIN files f
 ON f.rule_id=j.rule_id AND f.path=COALESCE(NULLIF(tf.source_path,''),tf.path)
 WHERE tf.job_id=j.job_id AND f.state='failed' AND f.source_present=1))`
}

type RuleFileFailure struct {
	Open, Ignored int
	Message       string
}

func (s *Store) RuleFileFailures(ctx context.Context) (map[string]RuleFileFailure, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rule_id,
 SUM(error_ignored=0),SUM(error_ignored=1),
 MAX(CASE WHEN error_ignored=0 THEN last_error ELSE '' END)
 FROM files WHERE state='failed' AND source_present=1 GROUP BY rule_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RuleFileFailure{}
	for rows.Next() {
		var id string
		var failure RuleFileFailure
		if err := rows.Scan(&id, &failure.Open, &failure.Ignored, &failure.Message); err != nil {
			return nil, err
		}
		out[id] = failure
	}
	return out, rows.Err()
}

// Ignoring acknowledges existing notifications only. Execution states,
// manifests, errors, source files and retry eligibility are preserved.
func (s *Store) SetRuleErrorsIgnored(ctx context.Context, ids []string, ignored bool) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	unique := make(map[string]bool, len(ids))
	for _, id := range ids {
		if unique[id] {
			continue
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM rules WHERE id=? AND is_deleted=0`, id).Scan(&exists); err != nil {
			return 0, err
		}
		unique[id] = true
	}
	var changed int64
	flag := boolToInt(ignored)
	for id := range unique {
		for _, query := range []string{
			`UPDATE files SET error_ignored=? WHERE rule_id=? AND state='failed' AND error_ignored<>?`,
			`UPDATE jobs SET error_ignored=? WHERE rule_id=? AND status='failed' AND error_ignored<>?`,
			`UPDATE rule_runtime SET scan_error_ignored=? WHERE rule_id=? AND scan_error<>'' AND scan_error_ignored<>?`,
		} {
			result, err := tx.ExecContext(ctx, query, flag, id, flag)
			if err != nil {
				return 0, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}
			changed += n
		}
	}
	return changed, tx.Commit()
}

func (s *Store) JobAttentionCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs j WHERE `+currentRuleTaskFailureSQL()+`
 AND j.error_ignored=0 AND (j.origin<>'scheduler' OR EXISTS(
 SELECT 1 FROM transfer_job_files tf JOIN files f
 ON f.rule_id=j.rule_id AND f.path=COALESCE(NULLIF(tf.source_path,''),tf.path)
 WHERE tf.job_id=j.job_id AND f.state='failed' AND f.source_present=1 AND f.error_ignored=0))`).Scan(&count)
	return count, err
}

// Keep the missing-rule error recognizable by HTTP callers without changing
// the store's transaction boundary.
var ErrAttentionRuleNotFound = sql.ErrNoRows
