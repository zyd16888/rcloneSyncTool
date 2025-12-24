package daemon

import (
	"context"
	"log"

	"115togd/internal/store"
)

func RecoverDanglingRuns(ctx context.Context, st *store.Store) error {
	// After restart we don't know whether previous rclone processes are still running,
	// so we mark them as failed and re-queue transferring files.
	_, err := st.DB().ExecContext(ctx, `
UPDATE jobs
SET status='failed',
    ended_at=strftime('%s','now'),
    error=CASE WHEN error='' THEN 'daemon restarted' ELSE error END
WHERE status='running'
`)
	if err != nil {
		return err
	}
	if _, err := st.DB().ExecContext(ctx, `
UPDATE files
SET state='queued', job_id=NULL
WHERE state='transferring'
`); err != nil {
		return err
	}
	log.Printf("recovered: marked running jobs failed and re-queued transferring files")
	return nil
}

