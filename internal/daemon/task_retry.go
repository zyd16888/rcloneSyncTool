package daemon

import (
	"context"
	"errors"
	"path/filepath"

	"115togd/internal/store"
)

func RetryTask(ctx context.Context, st *store.Store, source store.TransferJob, id, key string) (store.TransferJob, error) {
	spec, err := DecodeStoredTransferSpec(source.RequestJSON)
	if err != nil {
		return store.TransferJob{}, err
	}
	files, err := st.AllTaskFiles(ctx, source.JobID)
	if err != nil {
		return store.TransferJob{}, err
	}
	atomic := spec.RuleSnapshot != nil && spec.RuleSnapshot.AtomicPublish
	resumePublication := atomic && (source.Phase == "publishing" || source.Phase == "cleanup")
	if len(files) > 0 {
		if !atomic {
			var remaining []store.TransferJobFile
			for _, f := range files {
				if f.State != "done" {
					remaining = append(remaining, f)
				}
			}
			files = remaining
		}
		if len(files) == 0 {
			return store.TransferJob{}, errors.New("文件清单已全部完成")
		}
		spec.Files = files
	}
	if atomic && !resumePublication {
		spec.Files = nil
		spec.Prepared = false
	} else if !atomic {
		spec.Prepared = false
	}
	encoded, err := EncodeTransferSpec(spec)
	if err != nil {
		return store.TransferJob{}, err
	}
	settings, err := st.RuntimeSettings(ctx)
	if err != nil {
		return store.TransferJob{}, err
	}
	phase := "copying"
	if resumePublication {
		phase = source.Phase
	}
	retry := store.TransferJob{JobID: id, RuleID: source.RuleID, Origin: source.Origin, TransferMode: spec.Operation, IdempotencyKey: key, Fingerprint: source.Fingerprint, RequestJSON: encoded, LogPath: filepath.Join(settings.LogDir, source.RuleID, id+".log"), Phase: phase, Prepared: spec.Prepared, StagePath: source.StagePath, RetryOf: source.JobID, QuotaGroup: source.QuotaGroup}
	if err := st.CreateTaskRetry(ctx, source, retry, spec.Files); err != nil {
		return store.TransferJob{}, err
	}
	job, _, err := st.GetTransferJob(ctx, id)
	return job, err
}
