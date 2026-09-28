package daemon

import (
	"115togd/internal/store"
	"context"
)

func (w *ruleWorker) reconcileExisting(ctx context.Context, settings store.RuntimeSettings) error {
	rows, err := w.st.DB().QueryContext(ctx, `SELECT path,size FROM files WHERE rule_id=? AND state='done' AND source_present=1`, w.rule.ID)
	if err != nil {
		return err
	}
	var files []store.TransferJobFile
	for rows.Next() {
		var f store.TransferJobFile
		if err := rows.Scan(&f.Path, &f.Size); err != nil {
			rows.Close()
			return err
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(files) == 0 {
		return nil
	}
	exists, err := destinationExists(ctx, w.rule, settings)
	if err != nil {
		return err
	}
	entries := map[string]lsjsonEntry{}
	if exists {
		entries, err = listDestination(ctx, w.rule, settings)
		if err != nil {
			return err
		}
	}
	for _, f := range files {
		if e, ok := entries[f.Path]; !ok || e.Size != f.Size {
			if _, err := w.st.DB().ExecContext(ctx, `UPDATE files SET state='stable',job_id=NULL,last_error='' WHERE rule_id=? AND path=? AND state='done'`, w.rule.ID, f.Path); err != nil {
				return err
			}
		}
	}
	_, err = w.st.EnqueueStable(ctx, w.rule.ID, w.rule.BatchSize, 0)
	return err
}
