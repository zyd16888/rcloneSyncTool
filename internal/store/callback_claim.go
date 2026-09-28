package store

import (
	"context"
	"time"
)

func (s *Store) ClaimCallback(ctx context.Context, id string, maxAttempts int) (TransferJob, bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET callback_state='sending',callback_attempts=callback_attempts+1,callback_next_at=?,updated_at=?
WHERE job_id=? AND origin='api' AND status IN ('done','failed','terminated') AND callback_url<>''
AND callback_attempts<? AND callback_state NOT IN ('delivered','abandoned','invalid_url')
AND (callback_state<>'sending' OR callback_next_at<=?)`, time.Now().Add(30*time.Second).Unix(), nowUnix(), id, maxAttempts, nowUnix())
	if err != nil {
		return TransferJob{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return TransferJob{}, false, err
	}
	job, ok, err := s.GetTransferJob(ctx, id)
	return job, ok, err
}
