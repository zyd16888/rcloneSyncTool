package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"115togd/internal/daemon"
	"115togd/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// Opt-in browser fixture. The daemon queue is not started; all data and rclone
// metadata live in the test directory. /fixture-stop closes it cleanly.
func TestBrowserFixtureServer(t *testing.T) {
	if os.Getenv("RCLONE_SYNC_BROWSER_FIXTURE") != "1" {
		t.Skip("browser fixture is opt-in")
	}
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote")
	_ = os.MkdirAll(remote, 0o755)
	t.Setenv("RCLONE_SYNC_TEST_ROOT", remote)
	t.Setenv("RCLONE_SYNC_TEST_BINARY", "1")
	config := filepath.Join(dir, "rclone.conf")
	_ = os.WriteFile(config, []byte("[mock]\ntype = local\n"), 0o600)
	st, err := store.Open(filepath.Join(dir, "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(dir, "logs")
	_ = os.MkdirAll(logs, 0o755)
	if err := st.EnsureDefaultSettings(ctx, store.DefaultSettings{RcloneConfigPath: config, LogDir: logs, RcPortStart: 55720, RcPortEnd: 55800, GlobalMaxJobs: 2, Transfers: 2, Checkers: 2, MetricsInterval: time.Second, SchedulerTick: time.Second}); err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("local-test-pw"), bcrypt.DefaultCost)
	_ = st.SetSetting(ctx, authPasswordHashKey, string(hash))
	_ = st.UpsertLimitGroup(ctx, store.LimitGroup{Name: "媒体同步", DailyLimitBytes: 900 << 30})
	_ = st.UpsertExtensionPreset(ctx, store.ExtensionPreset{Name: "网页与临时文件", Extensions: ".url .html .tmp"})
	_ = st.UpsertExtensionPreset(ctx, store.ExtensionPreset{Name: "下载中的文件", Extensions: ".part .aria2"})
	rules := []store.Rule{
		{ID: "电影归档", SrcKind: "local", SrcLocalRoot: filepath.Join(dir, "source"), DstRemote: "mock", DstPath: "/library", TransferMode: "move", Enabled: true, GroupByDirectory: true, AtomicPublish: true, StableSeconds: 60, LimitGroup: "媒体同步", APIEnabled: true},
		{ID: "等待配额", SrcKind: "local", SrcLocalRoot: dir, DstRemote: "mock", DstPath: "/queued", TransferMode: "copy", Enabled: true},
		{ID: "暂停但任务运行", SrcKind: "local", SrcLocalRoot: dir, DstRemote: "mock", DstPath: "/other", TransferMode: "copy", Enabled: false},
		{ID: "影片补传", SrcKind: "local", SrcLocalRoot: dir, DstRemote: "mock", DstPath: "/archive", TransferMode: "move", Enabled: true, GroupByDirectory: true, AtomicPublish: true},
	}
	for i := range rules {
		if err := st.UpsertRule(ctx, rules[i]); err != nil {
			t.Fatal(err)
		}
		rules[i], _, _ = st.GetRule(ctx, rules[i].ID)
		_ = st.StartRuleScan(ctx, rules[i].ID)
		_ = st.FinishRuleScan(ctx, rules[i].ID, store.ScanStats{Discovered: 120, Eligible: 80, Filtered: 40}, "")
	}
	_ = st.SetRuleBlock(ctx, "等待配额", "quota_exhausted", "整组文件超过剩余配额，等待配额恢复")
	_ = os.WriteFile(filepath.Join(dir, "daemon.log"), []byte("2026/09/28 13:00:00 INFO 服务已启动\n2026/09/28 13:01:00 WARN 一个任务等待配额\n2026/09/28 13:02:00 ERROR 一个目录发布失败\n"), 0o600)
	rc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"transferring": []map[string]any{{"name": "Movie.Part2.mkv", "size": int64(8 << 30), "bytes": int64(2 << 30), "speed": 34.2 * 1024 * 1024, "eta": 180}}})
	}))
	defer rc.Close()
	rcPort := rc.Listener.Addr().(*net.TCPAddr).Port
	var manifest []store.TransferJobFile
	for i := 0; i < 420; i++ {
		manifest = append(manifest, store.TransferJobFile{Path: fmt.Sprintf("extras/file-%03d.txt", i), Size: 1024})
	}
	manifest = append(manifest, store.TransferJobFile{Path: "Movie.Part1.mkv", Size: 8 << 30}, store.TransferJobFile{Path: "Movie.Part2.mkv", Size: 8 << 30}, store.TransferJobFile{Path: "poster.jpg", Size: 1024})
	create := func(id string, rule store.Rule, title, status, phase string, files []store.TransferJobFile, bytes, reused int64) {
		spec, _ := daemon.EncodeTransferSpec(daemon.TransferSpec{Operation: rule.TransferMode, SourceSubpath: title, DestinationSubpath: title, RuleSnapshot: &rule, Prepared: true})
		log := filepath.Join(logs, id+".log")
		_ = os.WriteFile(log, []byte("{\"level\":\"info\",\"msg\":\"File list prepared\",\"time\":\"2026-09-28T13:00:00Z\"}\n{\"level\":\"warning\",\"msg\":\"Waiting for quota\"}\n"), 0o600)
		_ = st.CreateTransferJob(ctx, store.TransferJob{JobID: id, RuleID: rule.ID, TransferMode: rule.TransferMode, RequestJSON: spec, Prepared: true, LogPath: log, Phase: phase}, files)
		var planned int64
		for _, f := range files {
			planned += f.Size
		}
		result, _ := json.Marshal(map[string]any{"progress": daemon.TaskProgress{PlannedBytes: planned, ReusedBytes: reused, ReadyBytes: reused + bytes, Initialized: true}})
		seedStatus := status
		seedBytes := bytes
		if status == "done" || status == "failed" {
			seedStatus = "running"
			seedBytes = 0
		}
		_, _ = st.DB().Exec("UPDATE jobs SET status=?,phase=?,started_at=?,rc_port=?,bytes_done=?,result_snapshot=? WHERE job_id=?", seedStatus, phase, time.Now().Add(-5*time.Minute).Unix(), rcPort, seedBytes, string(result), id)
		if seedBytes > 0 {
			_, _ = st.DB().Exec("INSERT INTO transfer_usage(job_id,rule_id,quota_group,ts,bytes) VALUES(?,?,?,?,?)", id, rule.ID, rule.LimitGroup, time.Now().UnixMilli(), seedBytes)
		}
		if status == "done" || status == "failed" {
			message := ""
			if status == "failed" {
				message = "正式影片目录已存在，暂存内容和源文件已保留"
			}
			_ = st.CompleteTask(ctx, id, status, bytes, 26.8*1024*1024, message, nil, map[string]any{"progress": daemon.TaskProgress{PlannedBytes: planned, ReusedBytes: reused, ReadyBytes: reused + bytes, Initialized: true}})
		}
	}
	create("ui-job", rules[2], "Movie.2024", "running", "copying", manifest, 2<<30, 8<<30)
	create("ui-verify", rules[0], "Interstellar.2014", "running", "verifying", []store.TransferJobFile{{Path: "Part1.mkv", Size: 8 << 30}, {Path: "Part2.mkv", Size: 8 << 30}}, 16<<30, 0)
	create("ui-waiting", rules[1], "Planet.Earth.III", "blocked", "copying", []store.TransferJobFile{{Path: "Episode01.mkv", Size: 3 << 30}}, 0, 0)
	create("ui-failed", rules[3], "Arrival.2016", "failed", "publishing", []store.TransferJobFile{{Path: "Arrival-1.mkv", Size: 6 << 30}, {Path: "Arrival-2.mkv", Size: 6 << 30}}, 8<<30, 4<<30)
	create("ui-done", rules[0], "Dune.2021", "done", "completed", []store.TransferJobFile{{Path: "Dune.mkv", Size: 7 << 30}}, 7<<30, 0)
	tickerDone := make(chan struct{})
	defer close(tickerDone)
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tickerDone:
				return
			case at := <-tick.C:
				_ = st.InsertJobMetric(context.Background(), store.JobMetric{JobID: "ui-job", Ts: at, Bytes: 2 << 30, Speed: 34.2 * 1024 * 1024, Transfers: 1})
			}
		}
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:18089")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	handler := New(st, daemon.NewSupervisor(st), logs, filepath.Join(dir, "daemon.log"))
	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"password": {"local-test-pw"}}.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, login)
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == authCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("fixture login failed")
	}
	var server *http.Server
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixture-stop" && r.Method == http.MethodPost {
			fmt.Fprint(w, "stopped")
			go func() {
				stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = server.Shutdown(stopCtx)
			}()
			return
		}
		if r.URL.Path != "/login" {
			r.AddCookie(cookie)
		}
		handler.ServeHTTP(w, r)
	})
	server = &http.Server{Handler: wrapped}
	fmt.Println("Browser fixture ready: http://127.0.0.1:18089")
	fmt.Println("Browser fixture temporary directory: " + dir)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
