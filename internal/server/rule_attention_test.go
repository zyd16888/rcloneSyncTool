package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"115togd/internal/store"
)

func attentionPost(t *testing.T, session *pageSession, path string, ids ...string) *httptest.ResponseRecorder {
	t.Helper()
	values := url.Values{"id": ids, "return_url": {"/rules?runtime=failed"}}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Requested-With", "XMLHttpRequest")
	request.AddCookie(&http.Cookie{Name: authCookieName, Value: session.cookie})
	response := httptest.NewRecorder()
	session.handler.ServeHTTP(response, request)
	return response
}

func seedPageFailure(t *testing.T, session *pageSession, id string) {
	t.Helper()
	ctx := context.Background()
	if err := session.store.UpsertRule(ctx, store.Rule{ID: id, SrcKind: "local", SrcLocalRoot: t.TempDir(), DstRemote: "mock", DstPath: "/library"}); err != nil {
		t.Fatal(err)
	}
	if err := session.store.CreateTransferJob(ctx, store.TransferJob{JobID: id + "-job", RuleID: id, Origin: store.OriginAPI, TransferMode: "copy"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := session.store.CompleteTask(ctx, id+"-job", store.TransferStatusFailed, 8, 0, "historical error", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRuleIgnoreAndRestoreUpdateFilteringWithoutChangingHistory(t *testing.T) {
	session := newPageTestServer(t)
	seedPageFailure(t, session, "old-failure")
	before := session.get(t, "/rules?runtime=failed")
	if before.Code != 200 || !strings.Contains(before.Body.String(), `data-row-id="old-failure"`) || !strings.Contains(before.Body.String(), "忽略本页错误") {
		t.Fatalf("missing failure or ignore action: %d %s", before.Code, before.Body.String())
	}
	response := attentionPost(t, session, "/rules/ignore_errors", "old-failure")
	if response.Code != 200 || response.Header().Get("Location") != "" {
		t.Fatalf("ignore did not return AJAX feedback: %d %s", response.Code, response.Body.String())
	}
	after := session.get(t, "/rules?runtime=failed&partial=1")
	var fragment struct {
		HTML string `json:"html"`
	}
	if after.Code != 200 || json.Unmarshal(after.Body.Bytes(), &fragment) != nil || strings.Contains(fragment.HTML, `data-row-id="old-failure"`) || !strings.Contains(fragment.HTML, "暂无匹配的规则") {
		t.Fatalf("ignored rule remained in attention filter: %d %s", after.Code, after.Body.String())
	}
	all := session.get(t, "/rules")
	if !strings.Contains(all.Body.String(), "旧错误提示已忽略") || !strings.Contains(all.Body.String(), "/rules/restore_errors") || strings.Contains(all.Body.String(), `title="historical error"`) {
		t.Fatalf("ignore indication or restoration missing: %s", all.Body.String())
	}
	job, _, _ := session.store.GetTransferJob(context.Background(), "old-failure-job")
	if job.Status != store.TransferStatusFailed || job.Error != "historical error" || job.BytesDone != 8 {
		t.Fatalf("ignoring changed history: %+v", job)
	}
	var stats struct {
		Counts map[string]int `json:"statusCounts"`
	}
	if err := json.Unmarshal(session.get(t, "/api/stats/now").Body.Bytes(), &stats); err != nil || stats.Counts["failed"] != 1 || stats.Counts["attention"] != 0 {
		t.Fatalf("history and attention counters disagree: %+v %v", stats, err)
	}
	response = attentionPost(t, session, "/rules/restore_errors", "old-failure")
	if response.Code != 200 || !strings.Contains(session.get(t, "/rules?runtime=failed").Body.String(), `data-row-id="old-failure"`) {
		t.Fatal("restored error did not return to the attention filter")
	}
}

func TestIgnoreBatchAndInvalidRuleAreAtomic(t *testing.T) {
	session := newPageTestServer(t)
	for _, id := range []string{"first", "second"} {
		seedPageFailure(t, session, id)
	}
	response := attentionPost(t, session, "/rules/ignore_errors", "first", "missing")
	if response.Code != 404 {
		t.Fatalf("invalid rule accepted: %d %s", response.Code, response.Body.String())
	}
	if n, _ := session.store.JobAttentionCount(context.Background()); n != 2 {
		t.Fatal("invalid batch partially ignored notifications")
	}
	response = attentionPost(t, session, "/rules/ignore_errors", "first", "second", "first")
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if n, _ := session.store.JobAttentionCount(context.Background()); n != 0 {
		t.Fatal("batch ignore left existing notifications active")
	}
}

func TestUnretryableHistoryReturnsUsefulFeedback(t *testing.T) {
	session := newPageTestServer(t)
	seedPageFailure(t, session, "legacy")
	response := session.post(t, "/rules/retry_failed", url.Values{"id": {"legacy"}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("native feedback redirect changed: %d", response.Code)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil || location.Query().Get("error") != "1" || !strings.Contains(location.Query().Get("notice"), "忽略当前错误提示") {
		t.Fatalf("unretryable history falsely reported success: %s %v", response.Header().Get("Location"), err)
	}
}

func TestCurrentFailureMessagePrecedesAnOlderBlockReason(t *testing.T) {
	session := newPageTestServer(t)
	seedPageFailure(t, session, "mixed")
	if err := session.store.SetRuleBlock(context.Background(), "mixed", "quota_exhausted", "old quota message"); err != nil {
		t.Fatal(err)
	}
	response := session.get(t, "/rules?runtime=failed")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `title="historical error"`) || strings.Contains(response.Body.String(), `title="old quota message"`) {
		t.Fatalf("old block hid the current failure: %d %s", response.Code, response.Body.String())
	}
}
