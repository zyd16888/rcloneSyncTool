package store

import (
	"context"
	"testing"
	"time"
)

func TestRuleAttentionFollowsCurrentFilesAndIndependentTasks(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	rule := seedTaskRule(t, st, "movies", 0)
	if err := st.UpsertScanEntries(ctx, rule, []ScanEntry{{Path: "film/Movie.mkv", Size: 20, ModTime: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	files := []TransferJobFile{{Path: "Movie.mkv", SourcePath: "film/Movie.mkv", Size: 20}}
	job := TransferJob{JobID: "original", RuleID: rule.ID, Origin: "scheduler", TransferMode: "copy", Prepared: true}
	if err := st.CreateTransferJob(ctx, job, files); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePreparedTask(ctx, job.JobID, "", files, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteTask(ctx, job.JobID, TransferStatusFailed, 0, 0, "network unavailable", nil, nil); err != nil {
		t.Fatal(err)
	}
	activity, err := st.RuleTransferActivities(ctx, time.Now().Add(-time.Minute))
	if err != nil || activity[rule.ID].Failed != 1 {
		t.Fatalf("current failure missing: %+v %v", activity, err)
	}
	if n, err := st.RetryFailed(ctx, rule.ID, 100); err != nil || n != 1 {
		t.Fatalf("failed files were not retried: %d %v", n, err)
	}
	activity, err = st.RuleTransferActivities(ctx, time.Now().Add(-time.Minute))
	if err != nil || activity[rule.ID].Failed != 0 || activity[rule.ID].LatestError != "" {
		t.Fatalf("historical failure still requires attention: %+v %v", activity, err)
	}
	for _, id := range []string{"api-failed", "api-blocked"} {
		if err := st.CreateTransferJob(ctx, TransferJob{JobID: id, RuleID: rule.ID, Origin: OriginAPI, TransferMode: "copy"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CompleteTask(ctx, "api-failed", TransferStatusFailed, 0, 0, "source unavailable", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.BlockTransferJob(ctx, "api-blocked", "quota_exhausted", "waiting for quota"); err != nil {
		t.Fatal(err)
	}
	activity, err = st.RuleTransferActivities(ctx, time.Now().Add(-time.Minute))
	if err != nil || activity[rule.ID].Failed != 1 || activity[rule.ID].Blocked != 1 {
		t.Fatalf("independent failures and waits were merged: %+v %v", activity, err)
	}
}
