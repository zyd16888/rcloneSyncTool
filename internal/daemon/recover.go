package daemon

import (
	"context"
	"log"

	"115togd/internal/store"
)

func RecoverDanglingRuns(ctx context.Context, st *store.Store) error {
	// After restart we don't know whether previous rclone processes are still running,
	// so we mark them as failed and re-queue transferring files.
	type row struct {
		JobID   string
		LogPath string
	}
	var running []row

	rows, err := st.DB().QueryContext(ctx, `
SELECT job_id, log_path
FROM jobs
WHERE status='running'
`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.JobID, &r.LogPath); err != nil {
			_ = rows.Close()
			return err
		}
		running = append(running, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	_, err = st.DB().ExecContext(ctx, `
UPDATE jobs
SET status='failed',
    ended_at=strftime('%s','now'),
    error=CASE WHEN error='' THEN 'daemon restarted' ELSE error END
WHERE status='running'
`)
	if err != nil {
		return err
	}

	for _, j := range running {
		doneSet, _ := transferredPathsFromLog(j.LogPath)
		var donePaths []string
		if len(doneSet) > 0 {
			frows, err := st.DB().QueryContext(ctx, `
SELECT path
FROM files
WHERE job_id=? AND state='transferring'
`, j.JobID)
			if err != nil {
				return err
			}
			for frows.Next() {
				var p string
				if err := frows.Scan(&p); err != nil {
					_ = frows.Close()
					return err
				}
				if _, ok := doneSet[p]; ok {
					donePaths = append(donePaths, p)
				}
			}
			if err := frows.Close(); err != nil {
				return err
			}
		}
		_ = st.FinalizeJobFiles(ctx, j.JobID, donePaths, "queued", "")
		_ = st.ClearJobOnDone(ctx, j.JobID)
	}

	// Safety net: any remaining transferring rows without a running job record.
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
