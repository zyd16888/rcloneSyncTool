package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func seedAttentionFailure(t *testing.T, st *Store, rule Rule, id string) {
	t.Helper()
	ctx := context.Background()
	files := []TransferJobFile{{Path: "Movie.mkv", SourcePath: "Movie.mkv", Size: 20}}
	job := TransferJob{JobID: id, RuleID: rule.ID, Origin: OriginScheduler, TransferMode: "copy", Prepared: true}
	if err := st.CreateTransferJob(ctx, job, files); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePreparedTask(ctx, id, "", files, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteTask(ctx, id, TransferStatusFailed, 5, 0, "network unavailable", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestIgnoredErrorsSurviveRestartAndNewFailuresReappear(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "attention.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rule := seedTaskRule(t, st, "movies", 0)
	source := filepath.Join(rule.SrcLocalRoot, "Movie.mkv")
	content := []byte("source stays intact")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertScanEntries(ctx, rule, []ScanEntry{{Path: "Movie.mkv", Size: 20, ModTime: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	seedAttentionFailure(t, st, rule, "first")
	if err := st.StartRuleScan(ctx, rule.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRuleScan(ctx, rule.ID, ScanStats{}, "scan unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRuleErrorsIgnored(ctx, []string{rule.ID}, true); err != nil {
		t.Fatal(err)
	}
	job, _, _ := st.GetTransferJob(ctx, "first")
	if job.Status != TransferStatusFailed || job.Error != "network unavailable" || job.BytesDone != 5 {
		t.Fatalf("ignoring changed task truth: %+v", job)
	}
	counts, _ := st.RuleFileCounts(ctx, rule.ID)
	if counts.Failed != 1 {
		t.Fatalf("failed source was removed or completed: %+v", counts)
	}
	if actual, err := os.ReadFile(source); err != nil || string(actual) != string(content) {
		t.Fatalf("ignoring changed source file: %q %v", actual, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	failures, err := st.RuleFileFailures(ctx)
	if err != nil || failures[rule.ID].Open != 0 || failures[rule.ID].Ignored != 1 {
		t.Fatalf("ignored file notification returned after restart: %+v %v", failures, err)
	}
	runtimes, _ := st.RuleRuntimes(ctx)
	if !runtimes[rule.ID].ScanErrorIgnored || runtimes[rule.ID].ScanError != "scan unavailable" {
		t.Fatalf("ignored scan was lost: %+v", runtimes)
	}
	activity, _ := st.RuleTransferActivities(ctx, time.Now().Add(-time.Minute))
	if activity[rule.ID].Failed != 0 || activity[rule.ID].IgnoredFailures != 1 || activity[rule.ID].LatestError != "" {
		t.Fatalf("ignored task notification returned: %+v", activity)
	}
	if count, err := st.JobAttentionCount(ctx); err != nil || count != 0 {
		t.Fatalf("overview still counts ignored failure: %d %v", count, err)
	}
	if err := st.StartRuleScan(ctx, rule.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRuleScan(ctx, rule.ID, ScanStats{}, "scan unavailable"); err != nil {
		t.Fatal(err)
	}
	runtimes, _ = st.RuleRuntimes(ctx)
	if runtimes[rule.ID].ScanErrorIgnored {
		t.Fatal("new occurrence of the same scan error stayed ignored")
	}
	if _, err := st.SetRuleErrorsIgnored(ctx, []string{rule.ID}, true); err != nil {
		t.Fatal(err)
	}
	seedAttentionFailure(t, st, rule, "second")
	failures, _ = st.RuleFileFailures(ctx)
	activity, _ = st.RuleTransferActivities(ctx, time.Now().Add(-time.Minute))
	if failures[rule.ID].Open != 1 || activity[rule.ID].Failed != 1 || activity[rule.ID].IgnoredFailures != 1 {
		t.Fatalf("new transfer failure stayed ignored: files=%+v tasks=%+v", failures, activity)
	}
	if _, err := st.SetRuleErrorsIgnored(ctx, []string{rule.ID}, false); err != nil {
		t.Fatal(err)
	}
	runtimes, _ = st.RuleRuntimes(ctx)
	if runtimes[rule.ID].ScanErrorIgnored {
		t.Fatal("scan reminder was not restored")
	}
}

func TestAttentionMigrationPreservesLegacyFailures(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	rule := seedTaskRule(t, st, "legacy", 0)
	if err := st.UpsertScanEntries(ctx, rule, []ScanEntry{{Path: "Movie.mkv", Size: 20, ModTime: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	seedAttentionFailure(t, st, rule, "legacy-task")
	_ = st.StartRuleScan(ctx, rule.ID)
	_ = st.FinishRuleScan(ctx, rule.ID, ScanStats{}, "legacy scan failure")
	// Recreate the v0.4.3 schema around populated records.
	for _, query := range []string{
		`ALTER TABLE files DROP COLUMN error_ignored`,
		`ALTER TABLE jobs DROP COLUMN error_ignored`,
		`ALTER TABLE rule_runtime DROP COLUMN scan_error_ignored`,
	} {
		if _, err := st.DB().ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	failures, _ := st.RuleFileFailures(ctx)
	runtimes, _ := st.RuleRuntimes(ctx)
	job, _, _ := st.GetTransferJob(ctx, "legacy-task")
	if failures[rule.ID].Open != 1 || runtimes[rule.ID].ScanErrorIgnored || job.Status != TransferStatusFailed || job.Error != "network unavailable" {
		t.Fatalf("migration changed old failures: %+v %+v %+v", failures, runtimes, job)
	}
}

func TestIgnoreBatchRollsBackForMissingRule(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	rule := seedTaskRule(t, st, "existing", 0)
	if err := st.CreateTransferJob(ctx, TransferJob{JobID: "api", RuleID: rule.ID, Origin: OriginAPI, TransferMode: "copy"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteTask(ctx, "api", TransferStatusFailed, 0, 0, "old failure", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRuleErrorsIgnored(ctx, []string{rule.ID, "missing"}, true); !errors.Is(err, ErrAttentionRuleNotFound) {
		t.Fatalf("missing batch rule accepted: %v", err)
	}
	if count, err := st.JobAttentionCount(ctx); err != nil || count != 1 {
		t.Fatalf("invalid batch partially ignored errors: %d %v", count, err)
	}
}

func TestMissingSourcesAndStoppedTasksRemainHistory(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	rule := seedTaskRule(t, st, "history", 0)
	if err := st.UpsertScanEntries(ctx, rule, []ScanEntry{{Path: "Movie.mkv", Size: 20, ModTime: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	files := []TransferJobFile{{Path: "Movie.mkv", SourcePath: "Movie.mkv", Size: 20}}
	if err := st.CreateTransferJob(ctx, TransferJob{JobID: "vanished", RuleID: rule.ID, Origin: OriginScheduler, TransferMode: "copy"}, files); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePreparedTask(ctx, "vanished", "", files, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartTransferJob(ctx, "vanished", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyScan(ctx, rule, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteTask(ctx, "vanished", TransferStatusFailed, 0, 0, "source missing", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTransferJob(ctx, TransferJob{JobID: "stopped", RuleID: rule.ID, Origin: OriginAPI, TransferMode: "copy"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteTask(ctx, "stopped", TransferStatusTerminated, 0, 0, "cancelled", nil, nil); err != nil {
		t.Fatal(err)
	}
	filesCount, _ := st.RuleFileCounts(ctx, rule.ID)
	failures, _ := st.RuleFileFailures(ctx)
	activity, _ := st.RuleTransferActivities(ctx, time.Now().Add(-time.Minute))
	attention, err := st.JobAttentionCount(ctx)
	if filesCount.Failed != 1 || failures[rule.ID].Open != 0 || activity[rule.ID].Failed != 0 || attention != 0 || err != nil {
		t.Fatalf("historical failures still require attention: files=%+v alerts=%+v tasks=%+v count=%d err=%v", filesCount, failures, activity, attention, err)
	}
	job, _, _ := st.GetTransferJob(ctx, "vanished")
	if job.Status != TransferStatusFailed || job.Error != "source missing" {
		t.Fatal("failure history was erased")
	}
}
