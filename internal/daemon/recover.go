package daemon

import (
	"115togd/internal/store"
	"context"
	"log"
)

func RecoverDanglingRuns(ctx context.Context, st *store.Store) error {
	// Jobs with frozen requests retain their phase and staging directory. They
	// resume through the same durable queue used for their first execution.
	if _, err := st.DB().ExecContext(ctx, `UPDATE jobs SET status='pending',reserved_bytes=0,rc_port=0,queue_next_at=0,error='daemon restarted; waiting to resume' WHERE status='running' AND request_snapshot<>''`); err != nil {
		return err
	}
	// Pre-upgrade tasks have no immutable manifest and cannot be replayed.
	if _, err := st.DB().ExecContext(ctx, `UPDATE jobs SET status='failed',ended_at=strftime('%s','now'),reserved_bytes=0,error='daemon restarted; retry source files' WHERE status='running' AND request_snapshot=''`); err != nil {
		return err
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE files SET state='queued' WHERE state='transferring' AND EXISTS(SELECT 1 FROM jobs WHERE job_id=files.job_id AND status IN ('pending','blocked'))`); err != nil {
		return err
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE files SET state='new',job_id=NULL WHERE state='transferring' AND NOT EXISTS(SELECT 1 FROM jobs WHERE job_id=files.job_id AND status IN ('pending','blocked','running'))`); err != nil {
		return err
	}
	if err := st.RepairFileBindings(ctx); err != nil {
		return err
	}
	log.Printf("recovered: resumable tasks queued; obsolete file bindings repaired")
	return nil
}
