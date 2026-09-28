package server

import (
	"context"
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

func TestBrowserFixtureServer(t *testing.T) {
	if os.Getenv("RCLONE_SYNC_BROWSER_FIXTURE") != "1" {
		t.Skip("browser fixture is opt-in")
	}
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote")
	_ = os.MkdirAll(remote, 0o755)
	t.Setenv("RCLONE_SYNC_TEST_ROOT", remote)
	t.Setenv("RCLONE_SYNC_TEST_BINARY", "1")
	st, err := store.Open(filepath.Join(dir, "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureDefaultSettings(ctx, store.DefaultSettings{LogDir: filepath.Join(dir, "logs"), RcPortStart: 55720, RcPortEnd: 55800, GlobalMaxJobs: 2, Transfers: 2, Checkers: 2, MetricsInterval: time.Second, SchedulerTick: time.Second}); err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("local-test-pw"), bcrypt.DefaultCost)
	_ = st.SetSetting(ctx, authPasswordHashKey, string(hash))
	for _, r := range []store.Rule{{ID: "电影归档", SrcKind: "local", SrcLocalRoot: filepath.Join(dir, "source"), DstRemote: "mock", DstPath: "/library", Enabled: true, GroupByDirectory: true, AtomicPublish: true, StableSeconds: 60}, {ID: "等待配额", SrcKind: "local", SrcLocalRoot: dir, DstRemote: "mock", DstPath: "/queued", Enabled: true}, {ID: "暂停但任务运行", SrcKind: "local", SrcLocalRoot: dir, DstRemote: "mock", DstPath: "/other", Enabled: false}} {
		if err := st.UpsertRule(ctx, r); err != nil {
			t.Fatal(err)
		}
		_ = st.StartRuleScan(ctx, r.ID)
		_ = st.FinishRuleScan(ctx, r.ID, store.ScanStats{Discovered: 120, Eligible: 80, Filtered: 40}, "")
	}
	_ = st.SetRuleBlock(ctx, "等待配额", "quota_exhausted", "完整影片组超过剩余配额，等待配额恢复")
	_ = st.CreateTransferJob(ctx, store.TransferJob{JobID: "ui-job", RuleID: "暂停但任务运行", TransferMode: "copy", Prepared: true}, []store.TransferJobFile{{Path: "Movie Part 一.mkv", Size: 1024}, {Path: "Movie Part 二.mkv", Size: 2048}})
	_, _ = st.StartTransferJob(ctx, "ui-job", 0)
	listener, err := net.Listen("tcp", "127.0.0.1:18089")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	fmt.Println("Browser fixture ready: http://127.0.0.1:18089")
	handler := New(st, daemon.NewSupervisor(st), filepath.Join(dir, "logs"), filepath.Join(dir, "daemon.log"))
	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"password": {"local-test-pw"}}.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	var cookie *http.Cookie
	for _, c := range loginResponse.Result().Cookies() {
		if c.Name == authCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("fixture login failed")
	}
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.AddCookie(cookie)
		if r.URL.Path == "/fixture-mobile" {
			fmt.Fprint(w, `<!doctype html><html><body style="margin:0;background:#ddd"><iframe src="/rules" title="390px rules viewport" style="border:0;width:390px;height:900px"></iframe></body></html>`)
			return
		}
		handler.ServeHTTP(w, r)
	})
	if err := http.Serve(listener, wrapped); err != nil {
		t.Fatal(err)
	}
}
