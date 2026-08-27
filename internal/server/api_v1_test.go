package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"115togd/internal/daemon"
	"115togd/internal/store"
	"115togd/internal/version"
)

func newTestServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	handler := New(st, daemon.NewSupervisor(st), filepath.Join(dir, "logs"), filepath.Join(dir, "daemon.log"))
	return handler, st
}

func doAPI(t *testing.T, handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return payload
}

func TestAPIV1RequiresBearerToken(t *testing.T) {
	handler, st := newTestServer(t)
	_, plaintext, err := st.CreateAPIToken(context.Background(), "workflow", time.Time{})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	for _, tc := range []struct {
		name   string
		header string
	}{
		{"missing", ""},
		{"wrong", "not-a-token"},
		{"prefix-only", "Bearer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d (%s)", rec.Code, rec.Body.String())
			}
			body := decode(t, rec)
			errObj, ok := body["error"].(map[string]any)
			if !ok || errObj["code"] != "unauthorized" {
				t.Fatalf("unexpected error body: %s", rec.Body.String())
			}
			if errObj["request_id"] == "" {
				t.Fatal("error body must carry a request id")
			}
		})
	}

	rec := doAPI(t, handler, http.MethodGet, "/api/v1/version", plaintext)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with a valid token, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(apiRequestIDHeader) == "" {
		t.Fatal("responses must echo a request id header")
	}
}

func TestAPIV1RejectsSessionCookie(t *testing.T) {
	handler, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "v1.forged.cookie.value"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a browser session must not reach the machine API, got %d", rec.Code)
	}
}

func TestAPIV1DisabledTokenLosesAccess(t *testing.T) {
	handler, st := newTestServer(t)
	ctx := context.Background()
	token, plaintext, err := st.CreateAPIToken(ctx, "workflow", time.Time{})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if rec := doAPI(t, handler, http.MethodGet, "/api/v1/health", plaintext); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 before disabling, got %d", rec.Code)
	}
	if err := st.SetAPITokenEnabled(ctx, token.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if rec := doAPI(t, handler, http.MethodGet, "/api/v1/health", plaintext); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after disabling, got %d", rec.Code)
	}
}

func TestAPIV1Version(t *testing.T) {
	handler, st := newTestServer(t)
	_, plaintext, _ := st.CreateAPIToken(context.Background(), "workflow", time.Time{})

	body := decode(t, doAPI(t, handler, http.MethodGet, "/api/v1/version", plaintext))
	if body["api_version"] != version.APIVersion {
		t.Fatalf("api_version must be the frozen contract version, got %v", body["api_version"])
	}
	if body["name"] != version.Name {
		t.Fatalf("unexpected name: %v", body["name"])
	}
	if _, ok := body["app_version"]; !ok {
		t.Fatal("app_version missing")
	}
	if _, ok := body["rclone_available"]; !ok {
		t.Fatal("rclone_available missing")
	}
}

func TestAPIV1CapabilitiesOnlyExposesOptedInRules(t *testing.T) {
	handler, st := newTestServer(t)
	ctx := context.Background()
	_, plaintext, _ := st.CreateAPIToken(ctx, "workflow", time.Time{})

	if err := st.UpsertRule(ctx, store.Rule{
		ID: "private", SrcKind: "local", SrcLocalRoot: "/data/private",
		DstRemote: "gdrive", DstPath: "/Private", TransferMode: "copy",
	}); err != nil {
		t.Fatalf("upsert private rule: %v", err)
	}
	if err := st.UpsertRule(ctx, store.Rule{
		ID: "local-to-gdrive", SrcKind: "local", SrcLocalRoot: "/data/watch",
		DstRemote: "gdrive", DstPath: "/Library", TransferMode: "move",
		LimitGroup: "gdrive_main", APIEnabled: true,
	}); err != nil {
		t.Fatalf("upsert api rule: %v", err)
	}

	body := decode(t, doAPI(t, handler, http.MethodGet, "/api/v1/capabilities", plaintext))
	if body["api_version"] != version.APIVersion {
		t.Fatalf("unexpected api_version: %v", body["api_version"])
	}

	rules, _ := body["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("expected only the opted-in rule, got %d", len(rules))
	}
	rule, _ := rules[0].(map[string]any)
	if rule["id"] != "local-to-gdrive" {
		t.Fatalf("wrong rule exposed: %v", rule["id"])
	}
	if rule["dst_remote"] != "gdrive" || rule["limit_group"] != "gdrive_main" {
		t.Fatalf("destination and quota group must be discoverable: %+v", rule)
	}
	ops, _ := rule["allowed_operations"].([]any)
	if len(ops) != 1 || ops[0] != "move" {
		t.Fatalf("allowed_operations must default to the rule mode, got %v", ops)
	}

	features, _ := body["features"].(map[string]any)
	if features["transfer_jobs"] != false {
		t.Fatal("transfer_jobs must stay false until the job API ships")
	}
	limits, _ := body["limits"].(map[string]any)
	if limits["max_files"] != float64(apiMaxFilesPerJob) {
		t.Fatalf("unexpected max_files: %v", limits["max_files"])
	}
}

func TestAPIV1Health(t *testing.T) {
	handler, st := newTestServer(t)
	ctx := context.Background()
	_, plaintext, _ := st.CreateAPIToken(ctx, "workflow", time.Time{})

	body := decode(t, doAPI(t, handler, http.MethodGet, "/api/v1/health", plaintext))
	if body["api_rules"] != float64(0) {
		t.Fatalf("expected no api rules yet, got %v", body["api_rules"])
	}
	if body["status"] != "degraded" {
		t.Fatal("a deployment with no API-enabled rule cannot accept work and must report degraded")
	}
	if body["database"] != "ok" {
		t.Fatalf("unexpected database state: %v", body["database"])
	}

	if err := st.UpsertRule(ctx, store.Rule{
		ID: "local-to-gdrive", SrcKind: "local", SrcLocalRoot: "/data/watch",
		DstRemote: "gdrive", DstPath: "/Library", TransferMode: "move", APIEnabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	body = decode(t, doAPI(t, handler, http.MethodGet, "/api/v1/health", plaintext))
	if body["api_rules"] != float64(1) {
		t.Fatalf("expected one api rule, got %v", body["api_rules"])
	}
}
