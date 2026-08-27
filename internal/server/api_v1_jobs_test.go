package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"115togd/internal/store"
)

func apiRequest(
	t *testing.T,
	handler http.Handler,
	method, path, token, body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func newJobTestServer(t *testing.T) (http.Handler, *store.Store, string, string) {
	t.Helper()
	handler, st := newTestServer(t)
	ctx := context.Background()
	logDir := filepath.Join(t.TempDir(), "logs")
	if err := st.SetSetting(ctx, "log_dir", logDir); err != nil {
		t.Fatalf("set log dir: %v", err)
	}
	if err := st.UpsertRule(ctx, store.Rule{
		ID: "local-to-gdrive", SrcKind: "local", SrcLocalRoot: t.TempDir(),
		DstRemote: "gdrive", DstPath: "/Library", TransferMode: "move",
		LimitGroup: "gdrive_main", APIEnabled: true,
	}); err != nil {
		t.Fatalf("upsert rule: %v", err)
	}
	if err := st.UpsertRule(ctx, store.Rule{
		ID: "private", SrcKind: "local", SrcLocalRoot: t.TempDir(),
		DstRemote: "gdrive", DstPath: "/Private", TransferMode: "copy",
	}); err != nil {
		t.Fatalf("upsert private rule: %v", err)
	}
	_, plaintext, err := st.CreateAPIToken(ctx, "workflow", time.Time{})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return handler, st, plaintext, logDir
}

const submitBody = `{"external_id":"node-run-1","rule_id":"local-to-gdrive","operation":"move",
"source_subpath":"Movie.Name","destination_subpath":"incoming/run-1",
"files":[{"path":"movie.mkv","size":100}]}`

func submit(t *testing.T, handler http.Handler, token, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return apiRequest(t, handler, http.MethodPost, "/api/v1/transfer-jobs", token, body,
		map[string]string{idempotencyKeyHeader: key})
}

func jobFrom(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := decode(t, rec)
	job, ok := body["job"].(map[string]any)
	if !ok {
		t.Fatalf("response has no job: %s", rec.Body.String())
	}
	return job
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := decode(t, rec)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error: %s", rec.Body.String())
	}
	return errObj["code"].(string)
}

// ── idempotency ─────────────────────────────────────────────────────────────

func TestCreateTransferJobIsIdempotentOnKey(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)

	first := submit(t, handler, token, "run-1:upload:1", submitBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", first.Code, first.Body.String())
	}
	created := jobFrom(t, first)
	if created["status"] != store.TransferStatusPending {
		t.Fatalf("a new job must start pending, got %v", created["status"])
	}

	// A client that timed out and retried must get the same job back, not a
	// second transfer.
	second := submit(t, handler, token, "run-1:upload:1", submitBody)
	if second.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay, got %d (%s)", second.Code, second.Body.String())
	}
	body := decode(t, second)
	if body["idempotent"] != true {
		t.Fatal("replay must be reported as idempotent")
	}
	if jobFrom(t, second)["job_id"] != created["job_id"] {
		t.Fatal("replay returned a different job")
	}
}

func TestReplayIgnoresKeyOrderAndWhitespace(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	first := submit(t, handler, token, "run-1:upload:1", submitBody)
	reordered := `{"rule_id":"local-to-gdrive","external_id":"node-run-1",
	"destination_subpath":"incoming/run-1","operation":"move",
	"files":[{"size":100,"path":"movie.mkv"}],"source_subpath":"Movie.Name"}`
	second := submit(t, handler, token, "run-1:upload:1", reordered)
	if second.Code != http.StatusOK {
		t.Fatalf("canonical replay must be accepted, got %d (%s)", second.Code, second.Body.String())
	}
	if jobFrom(t, second)["job_id"] != jobFrom(t, first)["job_id"] {
		t.Fatal("canonical replay returned a different job")
	}
}

func TestSameKeyWithDifferentRequestConflicts(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	submit(t, handler, token, "run-1:upload:1", submitBody)
	changed := strings.Replace(submitBody, "incoming/run-1", "incoming/run-2", 1)
	rec := submit(t, handler, token, "run-1:upload:1", changed)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
	if code := errorCode(t, rec); code != "idempotency_key_conflict" {
		t.Fatalf("unexpected code: %s", code)
	}
}

func TestExternalIDCannotBeReusedByAnotherKey(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	submit(t, handler, token, "run-1:upload:1", submitBody)
	rec := submit(t, handler, token, "run-1:upload:2", submitBody)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "external_id_conflict" {
		t.Fatalf("unexpected code: %s", code)
	}
}

func TestCreateTransferJobRequiresIdempotencyKey(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	rec := apiRequest(t, handler, http.MethodPost, "/api/v1/transfer-jobs", token, submitBody, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// ── rule and operation gating ───────────────────────────────────────────────

func TestRuleNotOpenedToAPIIsNotFound(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	body := strings.Replace(submitBody, "local-to-gdrive", "private", 1)
	rec := submit(t, handler, token, "run-1:upload:1", body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "rule_not_found" {
		t.Fatalf("unexpected code: %s", code)
	}
}

func TestOperationOutsideTheRuleAllowListIsRejected(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	body := strings.Replace(submitBody, `"operation":"move"`, `"operation":"copy"`, 1)
	rec := submit(t, handler, token, "run-1:upload:1", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "operation_not_allowed" {
		t.Fatalf("unexpected code: %s", code)
	}
}

func TestOperationAllowListCanWidenTheRule(t *testing.T) {
	handler, st, token, _ := newJobTestServer(t)
	rule, _, _ := st.GetRule(context.Background(), "local-to-gdrive")
	rule.APIAllowedOperations = "copy,move"
	if err := st.UpsertRule(context.Background(), rule); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	body := strings.Replace(submitBody, `"operation":"move"`, `"operation":"copy"`, 1)
	rec := submit(t, handler, token, "run-1:upload:1", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestArbitraryRcloneArgsAreNotAccepted(t *testing.T) {
	handler, st, token, _ := newJobTestServer(t)
	body := strings.Replace(submitBody, `"operation":"move"`,
		`"operation":"move","rclone_extra_args":"--drive-acknowledge-abuse"`, 1)
	rec := submit(t, handler, token, "run-1:upload:1", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	// The unknown field must be dropped, never persisted into the spec that the
	// daemon later turns into an rclone command line.
	job, _, err := st.GetTransferJob(context.Background(), jobFrom(t, rec)["job_id"].(string))
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if strings.Contains(job.RequestJSON, "acknowledge-abuse") {
		t.Fatalf("caller supplied rclone args leaked into the stored spec: %s", job.RequestJSON)
	}
}

// ── path safety ─────────────────────────────────────────────────────────────

func TestPathEscapesAreRejected(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	for name, body := range map[string]string{
		"source parent":      strings.Replace(submitBody, `"source_subpath":"Movie.Name"`, `"source_subpath":"../etc"`, 1),
		"source absolute":    strings.Replace(submitBody, `"source_subpath":"Movie.Name"`, `"source_subpath":"/etc"`, 1),
		"source remote":      strings.Replace(submitBody, `"source_subpath":"Movie.Name"`, `"source_subpath":"gdrive:/x"`, 1),
		"destination parent": strings.Replace(submitBody, `"destination_subpath":"incoming/run-1"`, `"destination_subpath":"a/../../b"`, 1),
		"file parent":        strings.Replace(submitBody, `"path":"movie.mkv"`, `"path":"../movie.mkv"`, 1),
		"file absolute":      strings.Replace(submitBody, `"path":"movie.mkv"`, `"path":"/movie.mkv"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			rec := submit(t, handler, token, "run-1:upload:"+name, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (%s)", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "path_escape" {
				t.Fatalf("unexpected code: %s", code)
			}
		})
	}
}

func TestNormalizeSubpath(t *testing.T) {
	for _, tc := range []struct{ in, out string }{
		{"", ""},
		{"a/b", "a/b"},
		{"./a//b/", "a/b"},
		{"a\\b", "a/b"},
	} {
		got, err := normalizeSubpath(tc.in)
		if err != nil || got != tc.out {
			t.Fatalf("normalizeSubpath(%q) = %q, %v", tc.in, got, err)
		}
	}
	for _, bad := range []string{"../x", "/x", "~/x", "C:/x", "gdrive:x", "a/../../b", "a\x00b"} {
		if _, err := normalizeSubpath(bad); err == nil {
			t.Fatalf("normalizeSubpath(%q) must fail", bad)
		}
	}
}

func TestSymlinkSourceEscapeIsRejected(t *testing.T) {
	handler, st, token, _ := newJobTestServer(t)
	ctx := context.Background()
	rule, _, _ := st.GetRule(ctx, "local-to-gdrive")

	outside := t.TempDir()
	link := filepath.Join(rule.SrcLocalRoot, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}
	body := strings.Replace(submitBody, `"source_subpath":"Movie.Name"`, `"source_subpath":"escape"`, 1)
	rec := submit(t, handler, token, "run-1:upload:1", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "path_escape" {
		t.Fatalf("unexpected code: %s", code)
	}
}

func TestFileListLimit(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	paths := make([]string, 0, apiMaxFilesPerJob+1)
	for i := 0; i <= apiMaxFilesPerJob; i++ {
		paths = append(paths, `{"path":"f`+strconv.Itoa(i)+`.mkv","size":1}`)
	}
	body := `{"rule_id":"local-to-gdrive","destination_subpath":"incoming","files":[` +
		strings.Join(paths, ",") + `]}`
	rec := submit(t, handler, token, "run-1:upload:1", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if code := errorCode(t, rec); code != "file_list_too_large" {
		t.Fatalf("unexpected code: %s", code)
	}
}

// ── lookup, files, logs ─────────────────────────────────────────────────────

func TestLookupByExternalID(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	created := jobFrom(t, submit(t, handler, token, "run-1:upload:1", submitBody))

	rec := doAPI(t, handler, http.MethodGet, "/api/v1/transfer-jobs/by-external-id/node-run-1", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if jobFrom(t, rec)["job_id"] != created["job_id"] {
		t.Fatal("by-external-id returned a different job")
	}

	missing := doAPI(t, handler, http.MethodGet, "/api/v1/transfer-jobs/by-external-id/unknown", token)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", missing.Code)
	}
}

func TestFilesArePagedByCursor(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	body := `{"rule_id":"local-to-gdrive","destination_subpath":"incoming","files":[
	{"path":"a.mkv","size":1},{"path":"b.mkv","size":2},{"path":"c.mkv","size":3}]}`
	created := jobFrom(t, submit(t, handler, token, "run-1:upload:1", body))
	jobID := created["job_id"].(string)

	first := decode(t, doAPI(t, handler, http.MethodGet,
		"/api/v1/transfer-jobs/"+jobID+"/files?limit=2", token))
	files, _ := first["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	cursor, _ := first["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("a full page must return a cursor")
	}

	second := decode(t, doAPI(t, handler, http.MethodGet,
		"/api/v1/transfer-jobs/"+jobID+"/files?limit=2&cursor="+cursor, token))
	files, _ = second["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("expected the remaining file, got %d", len(files))
	}
	if second["next_cursor"] != "" {
		t.Fatal("a short page must end the listing")
	}
}

func TestLogsArePagedAndRedacted(t *testing.T) {
	handler, st, token, _ := newJobTestServer(t)
	created := jobFrom(t, submit(t, handler, token, "run-1:upload:1", submitBody))
	jobID := created["job_id"].(string)
	job, _, err := st.GetTransferJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(job.LogPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "INFO : starting\nINFO : token abcdef123456 used\nINFO : done\n"
	if err := os.WriteFile(job.LogPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	payload := decode(t, doAPI(t, handler, http.MethodGet,
		"/api/v1/transfer-jobs/"+jobID+"/logs", token))
	lines, _ := payload["lines"].([]any)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	joined := ""
	for _, line := range lines {
		joined += line.(string)
	}
	if strings.Contains(joined, "abcdef123456") {
		t.Fatalf("log output must be redacted: %s", joined)
	}
	if payload["next_cursor"] == "" {
		t.Fatal("logs must return a cursor for incremental reads")
	}

	// Reading again from the cursor returns nothing new.
	next := decode(t, doAPI(t, handler, http.MethodGet,
		"/api/v1/transfer-jobs/"+jobID+"/logs?cursor="+payload["next_cursor"].(string), token))
	if lines, _ := next["lines"].([]any); len(lines) != 0 {
		t.Fatalf("expected no new lines, got %d", len(lines))
	}
}

// ── cancel and retry ────────────────────────────────────────────────────────

func TestCancelPendingJobIsIdempotent(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	created := jobFrom(t, submit(t, handler, token, "run-1:upload:1", submitBody))
	jobID := created["job_id"].(string)

	first := apiRequest(t, handler, http.MethodPost,
		"/api/v1/transfer-jobs/"+jobID+"/cancel", token, "", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", first.Code, first.Body.String())
	}
	if jobFrom(t, first)["status"] != store.TransferStatusTerminated {
		t.Fatalf("expected terminated, got %v", jobFrom(t, first)["status"])
	}

	second := apiRequest(t, handler, http.MethodPost,
		"/api/v1/transfer-jobs/"+jobID+"/cancel", token, "", nil)
	if second.Code != http.StatusOK {
		t.Fatalf("cancelling a terminal job must converge, got %d", second.Code)
	}
}

func TestRetryRequiresTerminalJobAndNewKey(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	created := jobFrom(t, submit(t, handler, token, "run-1:upload:1", submitBody))
	jobID := created["job_id"].(string)

	pending := apiRequest(t, handler, http.MethodPost,
		"/api/v1/transfer-jobs/"+jobID+"/retry", token, "",
		map[string]string{idempotencyKeyHeader: "run-1:upload:2"})
	if pending.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a pending job, got %d", pending.Code)
	}

	apiRequest(t, handler, http.MethodPost, "/api/v1/transfer-jobs/"+jobID+"/cancel", token, "", nil)

	noKey := apiRequest(t, handler, http.MethodPost,
		"/api/v1/transfer-jobs/"+jobID+"/retry", token, "", nil)
	if noKey.Code != http.StatusBadRequest {
		t.Fatalf("a retry without a key must be rejected, got %d", noKey.Code)
	}

	retry := apiRequest(t, handler, http.MethodPost,
		"/api/v1/transfer-jobs/"+jobID+"/retry", token, "",
		map[string]string{idempotencyKeyHeader: "run-1:upload:2"})
	if retry.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", retry.Code, retry.Body.String())
	}
	retried := jobFrom(t, retry)
	if retried["job_id"] == jobID {
		t.Fatal("a retry must create a new external job, not resurrect the old one")
	}
	if retried["status"] != store.TransferStatusPending {
		t.Fatalf("a retry must start pending, got %v", retried["status"])
	}
}

// ── authentication and capability advertisement ─────────────────────────────

func TestTransferJobEndpointsRequireAToken(t *testing.T) {
	handler, _, _, _ := newJobTestServer(t)
	rec := apiRequest(t, handler, http.MethodPost, "/api/v1/transfer-jobs", "", submitBody,
		map[string]string{idempotencyKeyHeader: "run-1:upload:1"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestCapabilitiesAdvertiseTheJobApi(t *testing.T) {
	handler, _, token, _ := newJobTestServer(t)
	body := decode(t, doAPI(t, handler, http.MethodGet, "/api/v1/capabilities", token))
	features, _ := body["features"].(map[string]any)
	for _, key := range []string{"transfer_jobs", "files_list", "logs_cursor", "cancel", "retry"} {
		if features[key] != true {
			t.Fatalf("feature %s must be advertised once the job API ships", key)
		}
	}
	if features["callback_hmac"] != false {
		t.Fatal("callback_hmac must stay false until callbacks ship")
	}
}

func TestStoredSpecKeepsOnlyValidatedFields(t *testing.T) {
	handler, st, token, _ := newJobTestServer(t)
	created := jobFrom(t, submit(t, handler, token, "run-1:upload:1", submitBody))
	job, _, err := st.GetTransferJob(context.Background(), created["job_id"].(string))
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal([]byte(job.RequestJSON), &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if spec["operation"] != "move" || spec["source_subpath"] != "Movie.Name" {
		t.Fatalf("unexpected stored spec: %s", job.RequestJSON)
	}
	if job.Origin != store.OriginAPI {
		t.Fatalf("API jobs must be tagged, got origin %q", job.Origin)
	}
}
