package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"115togd/internal/store"
)

func TestMain(m *testing.M) {
	if os.Getenv("RCLONE_SYNC_TEST_BINARY") == "1" {
		mockRclone()
		return
	}
	os.Exit(m.Run())
}
func mockArg(name string) string {
	for i, a := range os.Args {
		if a == name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}
func mockPath(p string) string {
	if strings.HasPrefix(p, "mock:") {
		return filepath.Join(os.Getenv("RCLONE_SYNC_TEST_ROOT"), filepath.FromSlash(strings.TrimPrefix(p, "mock:")))
	}
	return p
}
func mockDie(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
func mockRclone() {
	if len(os.Args) < 2 {
		mockDie(fmt.Errorf("missing command"))
	}
	args := os.Args[1:]
	for _, a := range args {
		if a == "--partial" || a == "--partial-dir" {
			mockDie(fmt.Errorf("unknown flag: %s", a))
		}
	}
	switch args[0] {
	case "version":
		fmt.Print("rclone mock\n")
		return
	case "listremotes":
		fmt.Print("mock:\n")
		return
	case "config":
		if data, err := os.ReadFile(filepath.Join(os.Getenv("RCLONE_SYNC_TEST_ROOT"), "mock-config.json")); err == nil {
			os.Stdout.Write(data)
			return
		}
		fmt.Print(`{"mock":{"type":"local"}}`)
		return
	case "backend":
		if len(args) < 3 || args[1] != "features" {
			mockDie(fmt.Errorf("unsupported backend command"))
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"Root": mockPath(args[2]), "Features": map[string]bool{"DirMove": true}})
		return
	case "mkdir":
		if err := os.MkdirAll(mockPath(args[1]), 0o755); err != nil {
			mockDie(err)
		}
		return
	case "lsjson":
		root := mockPath(args[1])
		if mockArg("--stat") == "" {
			for _, a := range args {
				if a == "--stat" {
					info, err := os.Stat(root)
					if err != nil {
						mockDie(err)
					}
					json.NewEncoder(os.Stdout).Encode(map[string]any{"Path": info.Name(), "Size": info.Size(), "IsDir": info.IsDir(), "ModTime": info.ModTime().UTC().Format(time.RFC3339Nano), "ID": "mock-dir"})
					return
				}
			}
		}
		if _, err := os.Stat(root); err != nil {
			mockDie(err)
		}
		dirsOnly := false
		for _, a := range args {
			if a == "--dirs-only" {
				dirsOnly = true
			}
		}
		entries := []map[string]any{}
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == root {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if dirsOnly && strings.Contains(filepath.ToSlash(rel), "/") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if dirsOnly != d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			entries = append(entries, map[string]any{"Path": filepath.ToSlash(rel), "Size": info.Size(), "ModTime": info.ModTime().UTC().Format(time.RFC3339Nano), "IsDir": d.IsDir()})
			return nil
		})
		if err != nil {
			mockDie(err)
		}
		json.NewEncoder(os.Stdout).Encode(entries)
		return
	case "copy", "move":
		src, dst := mockPath(args[1]), mockPath(args[2])
		list, err := os.ReadFile(mockArg("--files-from-raw"))
		if err != nil {
			mockDie(err)
		}
		root := os.Getenv("RCLONE_SYNC_TEST_ROOT")
		logFile := mockArg("--log-file")
		var copied int64
		count := 0
		paths := strings.FieldsFunc(strings.TrimSpace(string(list)), func(r rune) bool { return r == '\n' || r == '\r' })
		invocation, _ := os.OpenFile(filepath.Join(root, "copy-requests.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		json.NewEncoder(invocation).Encode(paths)
		invocation.Close()
		for _, name := range paths {
			from, to := filepath.Join(src, filepath.FromSlash(name)), filepath.Join(dst, filepath.FromSlash(name))
			info, err := os.Stat(from)
			if err != nil {
				mockDie(err)
			}
			if existing, e := os.Stat(to); e == nil && existing.Size() == info.Size() && existing.ModTime().Equal(info.ModTime()) {
				if args[0] == "move" {
					_ = os.Remove(from)
				}
				continue
			}
			data, err := os.ReadFile(from)
			if err != nil {
				mockDie(err)
			}
			if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
				mockDie(err)
			}
			if err := os.WriteFile(to, data, 0o600); err != nil {
				mockDie(err)
			}
			_ = os.Chtimes(to, info.ModTime(), info.ModTime())
			copied += info.Size()
			count++
			if args[0] == "move" {
				_ = os.Remove(from)
			}
			if _, e := os.Stat(filepath.Join(root, "add-source-once")); e == nil {
				_ = os.Remove(filepath.Join(root, "add-source-once"))
				_ = os.WriteFile(filepath.Join(src, "new-part.mkv"), []byte("arrived late"), 0o600)
			}
			if _, e := os.Stat(filepath.Join(root, "fail-copy-once")); e == nil {
				_ = os.Remove(filepath.Join(root, "fail-copy-once"))
				mockStats(logFile, copied, count)
				mockDie(fmt.Errorf("injected transfer failure"))
			}
		}
		mockStats(logFile, copied, count)
		return
	case "delete":
		root := mockPath(args[1])
		data, err := os.ReadFile(mockArg("--files-from-raw"))
		if err != nil {
			mockDie(err)
		}
		for _, p := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if p != "" {
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
					mockDie(err)
				}
			}
		}
		return
	default:
		mockDie(fmt.Errorf("unsupported mock command: %s", args[0]))
	}
}
func mockStats(p string, bytes int64, files int) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		mockDie(err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		mockDie(err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(map[string]any{"stats": map[string]any{"bytes": bytes, "transfers": files, "speed": 1}})
}

type executionFixture struct {
	st             *store.Store
	supervisor     *Supervisor
	source, remote string
	rule           store.Rule
}

func newExecutionFixture(t *testing.T, atomic bool) *executionFixture {
	t.Helper()
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	name := "rclone"
	if strings.HasSuffix(strings.ToLower(executable), ".exe") {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RCLONE_SYNC_TEST_BINARY", "1")
	src, remote := filepath.Join(dir, "source"), filepath.Join(dir, "remote")
	_ = os.MkdirAll(src, 0o755)
	_ = os.MkdirAll(remote, 0o755)
	t.Setenv("RCLONE_SYNC_TEST_ROOT", remote)
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureDefaultSettings(context.Background(), store.DefaultSettings{LogDir: filepath.Join(dir, "logs"), RcPortStart: 55720, RcPortEnd: 55800, GlobalMaxJobs: 2, Transfers: 2, Checkers: 2, MetricsInterval: 50 * time.Millisecond, SchedulerTick: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	r := store.Rule{ID: "film", SrcKind: "local", SrcLocalRoot: src, DstRemote: "mock", DstPath: "/library", TransferMode: "move", Enabled: true, MaxParallelJobs: 1, StableSeconds: 0, BatchSize: 1, GroupByDirectory: atomic, AtomicPublish: atomic}
	if err := st.UpsertRule(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r, _, _ = st.GetRule(context.Background(), r.ID)
	return &executionFixture{st: st, supervisor: NewSupervisor(st), source: src, remote: remote, rule: r}
}
func (f *executionFixture) write(t *testing.T, p, content string) {
	t.Helper()
	target := filepath.Join(f.source, filepath.FromSlash(p))
	_ = os.MkdirAll(filepath.Dir(target), 0o755)
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(target, old, old)
}
func (f *executionFixture) queue(t *testing.T) store.TransferJob {
	t.Helper()
	ctx := context.Background()
	settings, _ := f.st.RuntimeSettings(ctx)
	entries, err := scanRule(ctx, f.rule, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ApplyScan(ctx, f.rule, entries); err != nil {
		t.Fatal(err)
	}
	w := newRuleWorker(f.st, f.rule, f.supervisor.portManager, f.supervisor.globalLimiter, f.supervisor.jobs)
	w.plan(ctx)
	jobs, err := f.st.ClaimableTransferJobs(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("queued tasks: %d %v", len(jobs), err)
	}
	return jobs[0]
}
func (f *executionFixture) run(t *testing.T, job store.TransferJob) store.TransferJob {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f.supervisor.tryStartTransferJob(ctx, job)
	f.supervisor.taskWG.Wait()
	result, ok, err := f.st.GetTransferJob(context.Background(), job.JobID)
	if err != nil || !ok {
		t.Fatalf("result: %v %v", ok, err)
	}
	return result
}

func assertTransferResultContract(t *testing.T, st *store.Store, job store.TransferJob, subpath, destination string) {
	t.Helper()
	var result struct {
		Operation   string `json:"operation"`
		RuleID      string `json:"rule_id"`
		Phase       string `json:"phase"`
		Destination struct{ Remote, Root, Subpath, Path string }
		Totals      struct {
			FilesTotal  int   `json:"files_total"`
			FilesDone   int   `json:"files_done"`
			FilesFailed int   `json:"files_failed"`
			BytesDone   int64 `json:"bytes_done"`
		}
	}
	// Keep the original public result shape available to API and callback clients.
	if err := json.Unmarshal([]byte(job.ResultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if result.Operation != job.TransferMode || result.RuleID != job.RuleID || result.Phase != job.Phase {
		t.Fatalf("result identity changed: %s", job.ResultJSON)
	}
	if result.Destination.Remote != "mock" || result.Destination.Root != "/library" || result.Destination.Subpath != subpath || result.Destination.Path != destination {
		t.Fatalf("destination contract changed: %s", job.ResultJSON)
	}
	counts, err := st.TransferJobFileCounts(context.Background(), job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.FilesTotal != counts.Total || result.Totals.FilesDone != counts.Done || result.Totals.FilesFailed != counts.Failed || result.Totals.BytesDone != job.BytesDone {
		t.Fatalf("result totals differ from settled manifest: %s %+v", job.ResultJSON, counts)
	}
}

func TestExistingSourcePartsMoveInOneTask(t *testing.T) {
	f := newExecutionFixture(t, false)
	f.write(t, "Movie-1.mkv", "part-one")
	f.write(t, "Movie-2.mkv", "part-two")
	job := f.queue(t)
	files, _ := f.st.AllTaskFiles(context.Background(), job.JobID)
	if len(files) != 2 {
		t.Fatalf("split parts: %d", len(files))
	}
	result := f.run(t, job)
	if result.Status != store.TransferStatusDone {
		t.Fatalf("task failed: %+v", result)
	}
	for _, name := range []string{"Movie-1.mkv", "Movie-2.mkv"} {
		if _, err := os.Stat(filepath.Join(f.remote, "library", name)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(f.source, name)); !os.IsNotExist(err) {
			t.Fatal("move retained source")
		}
	}
	if result.BytesDone != 16 {
		t.Fatalf("short task lost final metrics: %d", result.BytesDone)
	}
	assertTransferResultContract(t, f.st, result, "", "mock:/library")
}
func TestAtomicMovieFailureRetryPublishesThenCleansSource(t *testing.T) {
	f := newExecutionFixture(t, true)
	f.rule.DailyLimitBytes = 16
	if err := f.st.UpsertRule(context.Background(), f.rule); err != nil {
		t.Fatal(err)
	}
	f.write(t, "Movie/Part 一.mkv", "part-one")
	f.write(t, "Movie/Part 二.mkv", "part-two")
	_ = os.WriteFile(filepath.Join(f.remote, "fail-copy-once"), []byte("1"), 0o600)
	failed := f.run(t, f.queue(t))
	if failed.Status != store.TransferStatusFailed {
		t.Fatalf("injection did not fail: %+v", failed)
	}
	assertTransferResultContract(t, f.st, failed, "Movie", "mock:/library/Movie")
	if _, err := os.Stat(filepath.Join(f.remote, "library", "Movie")); !os.IsNotExist(err) {
		t.Fatal("partial movie became visible")
	}
	for _, name := range []string{"Part 一.mkv", "Part 二.mkv"} {
		if _, err := os.Stat(filepath.Join(f.source, "Movie", name)); err != nil {
			t.Fatal("failed transfer removed source")
		}
	}
	retry, err := RetryTask(context.Background(), f.st, failed, newID(), "")
	if err != nil {
		t.Fatal(err)
	}
	result := f.run(t, retry)
	if result.Status != store.TransferStatusDone {
		t.Fatalf("retry failed: %+v", result)
	}
	for _, name := range []string{"Part 一.mkv", "Part 二.mkv"} {
		if _, err := os.Stat(filepath.Join(f.remote, "library", "Movie", name)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(f.source, "Movie", name)); !os.IsNotExist(err) {
			t.Fatal("published movie retained source")
		}
	}
	if result.BytesDone != 8 {
		t.Fatalf("retry retransferred completed stage file: %d", result.BytesDone)
	}
	assertTransferResultContract(t, f.st, result, "Movie", "mock:/library/Movie")
}
func TestAtomicPublishConflictKeepsSourceAndStaging(t *testing.T) {
	for _, sameSize := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-size-%v", sameSize), func(t *testing.T) {
			f := newExecutionFixture(t, true)
			f.write(t, "Movie/Part 1.mkv", "part-one")
			f.write(t, "Movie/Part 2.mkv", "part-two")
			target := filepath.Join(f.remote, "library", "Movie")
			_ = os.MkdirAll(target, 0o755)
			content := "different"
			if sameSize {
				content = "wrongone"
			}
			_ = os.WriteFile(filepath.Join(target, "Part 1.mkv"), []byte(content), 0o600)
			_ = os.WriteFile(filepath.Join(target, "Part 2.mkv"), []byte("wrongtwo"), 0o600)
			result := f.run(t, f.queue(t))
			if result.Status != store.TransferStatusFailed {
				t.Fatalf("conflict ignored: %+v", result)
			}
			for _, name := range []string{"Part 1.mkv", "Part 2.mkv"} {
				if _, err := os.Stat(filepath.Join(f.source, "Movie", name)); err != nil {
					t.Fatal("conflict removed source")
				}
			}
			data, err := os.ReadFile(filepath.Join(target, "Part 1.mkv"))
			if err != nil || string(data) != content {
				t.Fatal("conflict changed the existing target")
			}
		})
	}
}

func TestRetryResumesDirectoryAlreadyPublishedBeforeResultWasSaved(t *testing.T) {
	f := newExecutionFixture(t, true)
	f.write(t, "Movie/Part 1.mkv", "part-one")
	f.write(t, "Movie/Part 2.mkv", "part-two")
	job := f.queue(t)
	stagePath := joinRemotePath(f.rule.StagingPath, job.JobID)
	stage := filepath.Join(f.remote, filepath.FromSlash(stagePath))
	_ = os.MkdirAll(stage, 0o755)
	_ = os.WriteFile(filepath.Join(stage, "Part 1.mkv"), []byte("part-one"), 0o600)
	_ = os.WriteFile(filepath.Join(stage, "Part 2.mkv"), []byte("part-two"), 0o600)
	target := filepath.Join(f.remote, "library", "Movie")
	_ = os.MkdirAll(filepath.Dir(target), 0o755)
	if err := os.Rename(stage, target); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := f.st.DB().Exec(`UPDATE jobs SET phase='publishing',stage_path=? WHERE job_id=?`, stagePath, job.JobID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.CompleteTask(ctx, job.JobID, store.TransferStatusFailed, 16, 0, "post-publication verification failed", nil, nil); err != nil {
		t.Fatal(err)
	}
	failed, _, _ := f.st.GetTransferJob(ctx, job.JobID)
	retry, err := RetryTask(ctx, f.st, failed, newID(), "")
	if err != nil {
		t.Fatal(err)
	}
	result := f.run(t, retry)
	if result.Status != store.TransferStatusDone || result.BytesDone != 0 {
		t.Fatalf("published task did not resume cleanup: %+v", result)
	}
	for _, name := range []string{"Part 1.mkv", "Part 2.mkv"} {
		if _, err := os.Stat(filepath.Join(f.source, "Movie", name)); !os.IsNotExist(err) {
			t.Fatal("published task retained source")
		}
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Fatal("published task lost destination")
		}
	}
	assertTransferResultContract(t, f.st, result, "Movie", "mock:/library/Movie")
}
func TestBudgetSelectsSmallerTaskInsteadOfReturningWholeBatch(t *testing.T) {
	f := newExecutionFixture(t, false)
	f.rule.DailyLimitBytes = 12
	f.rule.TransferMode = "copy"
	f.rule.BatchSize = 100
	_ = f.st.UpsertRule(context.Background(), f.rule)
	f.write(t, "a-large.mkv", "this-movie-is-too-large")
	f.write(t, "b-small.mkv", "fits")
	job := f.queue(t)
	files, _ := f.st.AllTaskFiles(context.Background(), job.JobID)
	if len(files) != 1 || files[0].Path != "b-small.mkv" {
		t.Fatalf("budget did not select small task: %+v", files)
	}
	result := f.run(t, job)
	if result.Status != store.TransferStatusDone {
		t.Fatalf("small task blocked: %+v", result)
	}
}

func TestDirectoryChangedDuringCopyCannotPublish(t *testing.T) {
	f := newExecutionFixture(t, true)
	f.write(t, "Movie/Part 1.mkv", "one")
	f.write(t, "Movie/Part 2.mkv", "two")
	_ = os.WriteFile(filepath.Join(f.remote, "add-source-once"), []byte("1"), 0o600)
	result := f.run(t, f.queue(t))
	if result.Status != store.TransferStatusFailed {
		t.Fatalf("source change ignored: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(f.remote, "library", "Movie")); !os.IsNotExist(err) {
		t.Fatal("changed directory was published")
	}
	if _, err := os.Stat(filepath.Join(f.source, "Movie", "Part 1.mkv")); err != nil {
		t.Fatal("changed source was removed")
	}
}
func TestRestartKeepsFrozenPhaseAndStaging(t *testing.T) {
	f := newExecutionFixture(t, true)
	f.write(t, "Movie/Part 1.mkv", "one")
	f.write(t, "Movie/Part 2.mkv", "two")
	job := f.queue(t)
	_, _ = f.st.DB().Exec(`UPDATE jobs SET status='running',phase='cleanup',stage_path='/stage/original',reserved_bytes=12 WHERE job_id=?`, job.JobID)
	if err := RecoverDanglingRuns(context.Background(), f.st); err != nil {
		t.Fatal(err)
	}
	recovered, _, _ := f.st.GetTransferJob(context.Background(), job.JobID)
	if recovered.Status != "pending" || recovered.Phase != "cleanup" || recovered.StagePath != "/stage/original" || recovered.ReservedBytes != 0 {
		t.Fatalf("restart discarded progress: %+v", recovered)
	}
}
func TestStartupRechecksDoneFileMissingAtDestination(t *testing.T) {
	f := newExecutionFixture(t, false)
	f.rule.TransferMode = "copy"
	_ = f.st.UpsertRule(context.Background(), f.rule)
	f.write(t, "old.mkv", "existing movie")
	settings, _ := f.st.RuntimeSettings(context.Background())
	entries, err := scanRule(context.Background(), f.rule, settings)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.st.ApplyScan(context.Background(), f.rule, entries)
	_, _ = f.st.DB().Exec(`UPDATE files SET state='done' WHERE rule_id=?`, f.rule.ID)
	w := newRuleWorker(f.st, f.rule, f.supervisor.portManager, f.supervisor.globalLimiter, f.supervisor.jobs)
	w.doScan(context.Background())
	w.plan(context.Background())
	jobs, err := f.st.ClaimableTransferJobs(context.Background(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("missing destination stayed done: %d %v", len(jobs), err)
	}
	if result := f.run(t, jobs[0]); result.Status != "done" {
		t.Fatalf("existing source did not transfer: %+v", result)
	}
}
