package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"115togd/internal/daemon"
	"115togd/internal/store"
)

func TestJobSnapshotKeepsRetryProgressAndHidesExecutionSecrets(t *testing.T) {
	session := newPageTestServer(t)
	ctx := context.Background()
	rule := store.Rule{ID: "movie", SrcKind: "local", SrcLocalRoot: t.TempDir(), DstRemote: "mock", DstPath: "/library", GroupByDirectory: true, AtomicPublish: true, RcloneExtraArgs: "--drive-token SECRET_FIXTURE"}
	if err := session.store.UpsertRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	rule, _, _ = session.store.GetRule(ctx, rule.ID)
	spec, _ := daemon.EncodeTransferSpec(daemon.TransferSpec{Operation: "move", SourceSubpath: "影片 <2024>", DestinationSubpath: "影片 <2024>", RuleSnapshot: &rule, Prepared: true})
	job := store.TransferJob{JobID: "retry-view", RuleID: rule.ID, TransferMode: "move", RequestJSON: spec, Prepared: true}
	files := []store.TransferJobFile{{Path: "Part1.mkv", Size: 8}, {Path: "Part2.mkv", Size: 8}}
	if err := session.store.CreateTransferJob(ctx, job, files); err != nil {
		t.Fatal(err)
	}
	progress := daemon.TaskProgress{PlannedBytes: 16, ReadyBytes: 16, ReusedBytes: 8, BaseBytes: 0, Initialized: true}
	if err := session.store.CompleteTask(ctx, job.JobID, "done", 8, 4, "", []string{"Part1.mkv", "Part2.mkv"}, map[string]any{"progress": progress}); err != nil {
		t.Fatal(err)
	}
	response := session.get(t, "/api/job?id=retry-view")
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var payload struct {
		View jobView `json:"view"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	v := payload.View
	if v.Percent != 100 || v.ReadyBytes != 16 || v.ActualBytes != 8 || v.ReusedBytes != 8 || v.Title != "影片 <2024>" {
		t.Fatalf("retry was displayed as incomplete: %+v", v)
	}
	if strings.Contains(response.Body.String(), "SECRET_FIXTURE") {
		t.Fatal("execution credentials leaked into UI snapshot")
	}
	html := session.get(t, "/jobs/view?id=retry-view&partial=1")
	if html.Code != 200 || strings.Contains(html.Body.String(), "影片 <2024>") {
		t.Fatalf("title was not escaped: %s", html.Body.String())
	}
}

func TestJobListFiltersBeforePagingAndSortsFreshSpeed(t *testing.T) {
	session := newPageTestServer(t)
	ctx := context.Background()
	if err := session.store.UpsertRule(ctx, store.Rule{ID: "movie", SrcKind: "local", SrcLocalRoot: t.TempDir(), DstRemote: "mock", DstPath: "/library"}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"slow", "fast", "stale"} {
		files := []store.TransferJobFile{{Path: "电影/Part1.mkv", Size: 100}}
		if err := session.store.CreateTransferJob(ctx, store.TransferJob{JobID: id, RuleID: "movie", TransferMode: "copy", Prepared: true, Phase: "copying"}, files); err != nil {
			t.Fatal(err)
		}
		_, _ = session.store.StartTransferJob(ctx, id, 0)
		at := time.Now()
		if id == "stale" {
			at = at.Add(-time.Hour)
		}
		_ = session.store.InsertJobMetric(ctx, store.JobMetric{JobID: id, Ts: at, Bytes: 10, Speed: float64((i + 1) * 100)})
	}
	r := session.get(t, "/jobs?q="+url.QueryEscape("电影")+"&sort=speed&direction=desc&partial=1")
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	var data struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	fast, slow, stale := strings.Index(data.HTML, `data-row-id="fast"`), strings.Index(data.HTML, `data-row-id="slow"`), strings.Index(data.HTML, `data-row-id="stale"`)
	if fast < 0 || slow < fast || stale < slow {
		t.Fatalf("speed ordering or filename search failed: %s", data.HTML)
	}
}

func TestUIActionsReturnFeedbackWithoutRedirecting(t *testing.T) {
	session := newPageTestServer(t)
	ctx := context.Background()
	_ = session.store.UpsertRule(ctx, store.Rule{ID: "movie", SrcKind: "local", SrcLocalRoot: t.TempDir(), DstRemote: "mock", DstPath: "/library", Enabled: true})
	request := httptest.NewRequest(http.MethodPost, "/rules/toggle", strings.NewReader(url.Values{"id": {"movie"}, "enabled": {"0"}, "return_url": {"/rules"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Requested-With", "XMLHttpRequest")
	request.AddCookie(&http.Cookie{Name: authCookieName, Value: session.cookie})
	response := httptest.NewRecorder()
	session.handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("Location") != "" || !strings.Contains(response.Body.String(), "message") {
		t.Fatalf("missing action feedback: %d %s", response.Code, response.Body.String())
	}
	rule, _, _ := session.store.GetRule(ctx, "movie")
	if rule.Enabled {
		t.Fatal("toggle did not persist")
	}
}

func TestManualFormKeepsGroupingAndSubpaths(t *testing.T) {
	session := newPageTestServer(t)
	ctx := context.Background()
	if err := session.store.UpsertLimitGroup(ctx, store.LimitGroup{Name: "shared", DailyLimitBytes: 1000}); err != nil {
		t.Fatal(err)
	}
	values := url.Values{
		"src_kind": {"local"}, "src_local_root": {t.TempDir()}, "dst_remote": {"mock"}, "dst_path": {"/library"},
		"transfer_mode": {"move"}, "group_by_directory": {"1"}, "atomic_publish": {"1"}, "staging_path": {"/staging"},
		"source_subpath": {"Movie.2024"}, "destination_subpath": {"Movie.2024"}, "limit_group": {"shared"},
	}
	post := func() *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/manual/start", strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("X-Requested-With", "XMLHttpRequest")
		request.AddCookie(&http.Cookie{Name: authCookieName, Value: session.cookie})
		response := httptest.NewRecorder()
		session.handler.ServeHTTP(response, request)
		return response
	}
	response := post()
	var feedback struct {
		Next string `json:"next"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &feedback) != nil {
		t.Fatalf("manual form submission failed: %d %s", response.Code, response.Body.String())
	}
	next, err := url.Parse(feedback.Next)
	if err != nil {
		t.Fatal(err)
	}
	job, found, err := session.store.GetTransferJob(ctx, next.Query().Get("id"))
	if err != nil || !found || job.Origin != store.OriginManual || job.QuotaGroup != "shared" {
		t.Fatalf("manual task metadata lost: %+v %v", job, err)
	}
	spec, err := daemon.DecodeStoredTransferSpec(job.RequestJSON)
	if err != nil || spec.SourceSubpath != "Movie.2024" || spec.DestinationSubpath != "Movie.2024" || spec.RuleSnapshot == nil || !spec.RuleSnapshot.AtomicPublish || !spec.RuleSnapshot.GroupByDirectory || spec.RuleSnapshot.StagingPath != "/staging" {
		t.Fatalf("manual policy was not frozen: %+v %v", spec, err)
	}
	search := session.get(t, "/jobs?q=Movie.2024&partial=1")
	if search.Code != 200 || !strings.Contains(search.Body.String(), job.JobID) {
		t.Fatalf("queued movie cannot be searched before preparation: %d %s", search.Code, search.Body.String())
	}
	values.Set("source_subpath", "../escape")
	response = post()
	if response.Code != 400 || !strings.Contains(response.Body.String(), `"field":"source_subpath"`) {
		t.Fatalf("unsafe subpath missing field feedback: %d %s", response.Code, response.Body.String())
	}
	if count, err := session.store.CountJobs(ctx); err != nil || count != 1 {
		t.Fatalf("invalid form created a task: %d %v", count, err)
	}
}
