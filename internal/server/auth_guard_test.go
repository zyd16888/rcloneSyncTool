package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"115togd/internal/store"
)

func TestUnauthenticatedManagementRequestsDoNotMutate(t *testing.T) {
	handler, st := newTestServer(t)
	ctx := context.Background()
	_ = st.UpsertRule(ctx, store.Rule{ID: "protected", SrcKind: "local", SrcLocalRoot: t.TempDir(), DstRemote: "mock", DstPath: "/library"})
	for _, test := range []struct {
		path   string
		values url.Values
	}{{"/rules/delete", url.Values{"id": {"protected"}}}, {"/rules/ignore_errors", url.Values{"id": {"protected"}}}, {"/rules/restore_errors", url.Values{"id": {"protected"}}}, {"/settings/save", url.Values{"global_max_jobs": {"99"}}}, {"/api-access/tokens/create", url.Values{"name": {"unauthorized"}}}} {
		req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d", test.path, rec.Code)
		}
	}
	if _, ok, _ := st.GetRule(ctx, "protected"); !ok {
		t.Fatal("unauthenticated request deleted rule")
	}
	if value, ok, _ := st.Setting(ctx, "global_max_jobs"); ok && value == "99" {
		t.Fatal("unauthenticated request saved settings")
	}
	tokens, _ := st.ListAPITokens(ctx)
	if len(tokens) != 0 {
		t.Fatal("unauthenticated request created token")
	}
}
func TestAuthenticatedForeignOriginCannotMutate(t *testing.T) {
	session := newPageTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api-access/tokens/create", strings.NewReader("name=foreign"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://foreign.example")
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: session.cookie})
	rec := httptest.NewRecorder()
	session.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin accepted: %d", rec.Code)
	}
	tokens, _ := session.store.ListAPITokens(context.Background())
	if len(tokens) != 0 {
		t.Fatal("foreign origin created token")
	}
}
func TestSizeUnitsAcceptDocumentedForms(t *testing.T) {
	for _, input := range []string{"1KB", "1KiB", "1 KB", "1K"} {
		n, err := parseSizeBytes(input)
		if err != nil || n != 1024 {
			t.Fatalf("%q: %d %v", input, n, err)
		}
	}
}
