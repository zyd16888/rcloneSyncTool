package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func seedTaskRule(t *testing.T, st *Store, id string, limit int64) Rule {
	t.Helper()
	r := Rule{ID: id, SrcKind: "local", SrcLocalRoot: t.TempDir(), DstRemote: "mock", DstPath: "/library", Enabled: true, StableSeconds: 0, BatchSize: 1, DailyLimitBytes: limit, MaxParallelJobs: 2}
	if err := st.UpsertRule(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r, _, _ = st.GetRule(context.Background(), id)
	return r
}
func TestFailedFileSurvivesRepeatedScansAndRetries(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	r := seedTaskRule(t, st, "retry", 0)
	entries := []ScanEntry{{Path: "movie.mkv", Size: 20, ModTime: time.Now().Add(-time.Hour)}}
	if err := st.UpsertScanEntries(ctx, r, entries); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJobRow(ctx, Job{JobID: "failed-task", RuleID: r.ID, TransferMode: "copy", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimQueuedForJob(ctx, r, "failed-task", 1); err != nil {
		t.Fatal(err)
	}
	_ = st.MarkJobFiles(ctx, "failed-task", "failed", "network unavailable")
	_ = st.UpdateJobFailed(ctx, "failed-task", "network unavailable", 0, 0)
	for i := 0; i < 3; i++ {
		if err := st.UpsertScanEntries(ctx, r, entries); err != nil {
			t.Fatal(err)
		}
	}
	counts, _ := st.RuleFileCounts(ctx, r.ID)
	if counts.Failed != 1 || counts.Queued != 0 {
		t.Fatalf("failure was overwritten: %+v", counts)
	}
	n, err := st.RetryFailed(ctx, r.ID, 100)
	if err != nil || n != 1 {
		t.Fatalf("retry count: %d %v", n, err)
	}
	if _, err := st.EnqueueStable(ctx, r.ID, 10, 0); err != nil {
		t.Fatal(err)
	}
	files, err := st.ClaimQueuedForJob(ctx, r, "new-task", 10)
	if err != nil || len(files) != 1 {
		t.Fatalf("retry not claimable: %v %v", files, err)
	}
}
func TestStartupRepairsOrphanedQueuedFile(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	r := seedTaskRule(t, st, "startup", 0)
	_ = st.UpsertScanEntries(ctx, r, []ScanEntry{{Path: "old.mkv", Size: 20, ModTime: time.Now().Add(-time.Hour)}})
	_, _ = st.DB().ExecContext(ctx, `UPDATE files SET job_id='missing-job',state='queued' WHERE rule_id=?`, r.ID)
	if err := st.RepairFileBindings(ctx); err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertScanEntries(ctx, r, []ScanEntry{{Path: "old.mkv", Size: 20, ModTime: time.Now().Add(-time.Hour)}})
	_, _ = st.EnqueueStable(ctx, r.ID, 10, 0)
	var binding string
	if err := st.DB().QueryRowContext(ctx, `SELECT COALESCE(job_id,'') FROM files WHERE rule_id=?`, r.ID).Scan(&binding); err != nil || binding != "" {
		t.Fatalf("stale binding: %q %v", binding, err)
	}
}
func TestPartsAreQueuedAsCompleteGroupPastBatchLimit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	r := seedTaskRule(t, st, "parts", 0)
	entries := []ScanEntry{{Path: "film/Movie-1.mkv", Size: 20, ModTime: time.Now().Add(-time.Hour)}, {Path: "film/Movie-2.mkv", Size: 25, ModTime: time.Now().Add(-time.Hour)}}
	if _, err := st.ApplyScan(ctx, r, entries); err != nil {
		t.Fatal(err)
	}
	groups, err := st.QueuedGroups(ctx, r)
	if err != nil || len(groups) != 1 || len(groups[0].Files) != 2 {
		t.Fatalf("split group: %+v %v", groups, err)
	}
}
func TestDirectoryWaitsForMarkerAndIncludesSidecars(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	r := seedTaskRule(t, st, "ready", 0)
	r.GroupByDirectory = true
	r.ReadyMarker = ".ready"
	r.MinFileSizeBytes = 10
	if err := st.UpsertRule(ctx, r); err != nil {
		t.Fatal(err)
	}
	entries := []ScanEntry{{Path: "film/Part 一.mkv", Size: 20, ModTime: time.Now().Add(-time.Hour)}, {Path: "film/Part 二.mkv", Size: 4, ModTime: time.Now().Add(-time.Hour)}, {Path: "film/movie.nfo", Size: 2, ModTime: time.Now().Add(-time.Hour)}}
	if _, err := st.ApplyScan(ctx, r, entries); err != nil {
		t.Fatal(err)
	}
	groups, _ := st.QueuedGroups(ctx, r)
	if len(groups) != 0 {
		t.Fatal("published before marker")
	}
	entries = append(entries, ScanEntry{Path: "film/.ready", Size: 0, ModTime: time.Now().Add(-time.Hour)})
	if _, err := st.ApplyScan(ctx, r, entries); err != nil {
		t.Fatal(err)
	}
	groups, err := st.QueuedGroups(ctx, r)
	if err != nil || len(groups) != 1 || len(groups[0].Files) != 3 {
		t.Fatalf("incomplete directory: %+v %v", groups, err)
	}
}
func TestSharedQuotaReservationsAreAtomicAcrossOrigins(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	r := seedTaskRule(t, st, "quota", 0)
	_ = st.UpsertLimitGroup(ctx, LimitGroup{Name: "shared", DailyLimitBytes: 100})
	r.LimitGroup = "shared"
	_ = st.UpsertRule(ctx, r)
	for _, job := range []TransferJob{{JobID: "api", RuleID: r.ID, Origin: OriginAPI, TransferMode: "copy"}, {JobID: "manual", RuleID: r.ID, Origin: OriginManual, TransferMode: "copy"}} {
		if err := st.CreateTransferJob(ctx, job, nil); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"api", "manual"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); results <- st.ReserveAndStartTask(ctx, id, 55720, r, 60) }(id)
	}
	wg.Wait()
	close(results)
	started, blocked := 0, 0
	for err := range results {
		if err == nil {
			started++
		} else if errors.Is(err, ErrQuotaUnavailable) {
			blocked++
		} else {
			t.Fatal(err)
		}
	}
	if started != 1 || blocked != 1 {
		t.Fatalf("quota overshoot: %d started, %d blocked", started, blocked)
	}
}
func TestWindowUsageCountsDeltasAndSurvivesRuleDeletion(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	r := seedTaskRule(t, st, "usage", 100)
	_ = st.CreateTransferJob(ctx, TransferJob{JobID: "usage", RuleID: r.ID, TransferMode: "copy"}, nil)
	_ = st.ReserveAndStartTask(ctx, "usage", 55720, r, 100)
	old := time.Now().Add(-25 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	if err := st.InsertJobMetric(ctx, JobMetric{JobID: "usage", Ts: old, Bytes: 60}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertJobMetric(ctx, JobMetric{JobID: "usage", Ts: recent, Bytes: 80}); err != nil {
		t.Fatal(err)
	}
	used, err := st.RuleUsageSince(ctx, r.ID, time.Now().Add(-24*time.Hour))
	if err != nil || used != 20 {
		t.Fatalf("window includes old bytes: %d %v", used, err)
	}
	budget, _ := st.RuleBudgetSince(ctx, r.ID, time.Now().Add(-24*time.Hour))
	if budget != 40 {
		t.Fatalf("reservation was double counted: %d", budget)
	}
	_ = st.CompleteTask(ctx, "usage", TransferStatusDone, 80, 0, "", nil, nil)
	if err := st.DeleteRule(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	used, _ = st.RuleUsageSince(ctx, r.ID, time.Now().Add(-24*time.Hour))
	if used != 20 {
		t.Fatalf("deletion erased quota: %d", used)
	}
}
