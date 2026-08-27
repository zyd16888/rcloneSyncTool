package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"115togd/internal/store"
)

// pageSession is one logged-in browser. The cookie is produced by the real
// login form, so these tests exercise the same session path a browser uses.
type pageSession struct {
	handler http.Handler
	store   *store.Store
	cookie  string
}

func newPageTestServer(t *testing.T) *pageSession {
	t.Helper()
	handler, st := newTestServer(t)

	login := url.Values{"password": {"pw-fixture-1"}, "password2": {"pw-fixture-1"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(login.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	cookie := ""
	for _, item := range rec.Result().Cookies() {
		if item.Name == authCookieName {
			cookie = item.Value
		}
	}
	if cookie == "" {
		t.Fatalf("login did not issue a session cookie: %d %s", rec.Code, rec.Body.String())
	}
	return &pageSession{handler: handler, store: st, cookie: cookie}
}

func (s *pageSession) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: s.cookie})
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func (s *pageSession) post(t *testing.T, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: s.cookie})
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func TestAPIAccessPageRequiresASession(t *testing.T) {
	session := newPageTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api-access", nil)
	rec := httptest.NewRecorder()
	session.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected a redirect to login, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "/login") {
		t.Fatalf("unexpected redirect: %s", rec.Header().Get("Location"))
	}
}

func TestCreateTokenRevealsThePlaintextExactlyOnce(t *testing.T) {
	session := newPageTestServer(t)

	rec := session.post(t, "/api-access/tokens/create", url.Values{"name": {"workflow"}, "expires_days": {"0"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "rst_") {
		t.Fatal("the created token must be shown once on the page")
	}
	// A secret must never travel through a URL, where it would land in history,
	// proxy logs and Referer headers.
	if rec.Header().Get("Location") != "" {
		t.Fatal("token creation must render directly, not redirect")
	}

	tokens, err := session.store.ListAPITokens(context.Background())
	if err != nil || len(tokens) != 1 {
		t.Fatalf("expected one stored token: %d %v", len(tokens), err)
	}
	plaintext := extractToken(t, body)
	if tokens[0].Hash != store.HashAPIToken(plaintext) {
		t.Fatal("stored hash does not match the revealed token")
	}

	// Reloading the page must not show it again.
	reload := session.get(t, "/api-access")
	if strings.Contains(reload.Body.String(), plaintext) {
		t.Fatal("a reload must not reveal the token again")
	}
	if !strings.Contains(reload.Body.String(), "workflow") {
		t.Fatal("the token name must remain listed")
	}
}

func TestTokenCreatedOnThePageAuthenticatesTheMachineApi(t *testing.T) {
	session := newPageTestServer(t)
	rec := session.post(t, "/api-access/tokens/create", url.Values{"name": {"workflow"}})
	plaintext := extractToken(t, rec.Body.String())

	api := doAPI(t, session.handler, http.MethodGet, "/api/v1/version", plaintext)
	if api.Code != http.StatusOK {
		t.Fatalf("a page-created token must work against /api/v1, got %d", api.Code)
	}
}

func TestTokenCanBeDisabledAndDeletedFromThePage(t *testing.T) {
	session := newPageTestServer(t)
	rec := session.post(t, "/api-access/tokens/create", url.Values{"name": {"workflow"}})
	plaintext := extractToken(t, rec.Body.String())
	tokens, _ := session.store.ListAPITokens(context.Background())
	id := tokens[0].ID

	session.post(t, "/api-access/tokens/toggle", url.Values{"id": {id}, "enabled": {"0"}})
	if got := doAPI(t, session.handler, http.MethodGet, "/api/v1/version", plaintext); got.Code != http.StatusUnauthorized {
		t.Fatalf("a disabled token must lose access, got %d", got.Code)
	}

	session.post(t, "/api-access/tokens/toggle", url.Values{"id": {id}, "enabled": {"1"}})
	if got := doAPI(t, session.handler, http.MethodGet, "/api/v1/version", plaintext); got.Code != http.StatusOK {
		t.Fatalf("re-enabling must restore access, got %d", got.Code)
	}

	session.post(t, "/api-access/tokens/delete", url.Values{"id": {id}})
	if got := doAPI(t, session.handler, http.MethodGet, "/api/v1/version", plaintext); got.Code != http.StatusUnauthorized {
		t.Fatalf("a deleted token must lose access, got %d", got.Code)
	}
}

func TestTokenCreationRejectsAnEmptyName(t *testing.T) {
	session := newPageTestServer(t)
	rec := session.post(t, "/api-access/tokens/create", url.Values{"name": {"  "}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "请填写 Token 名称") {
		t.Fatalf("expected an inline error, got %d", rec.Code)
	}
	tokens, _ := session.store.ListAPITokens(context.Background())
	if len(tokens) != 0 {
		t.Fatal("no token may be created without a name")
	}
}

func TestExpiryEnteredOnThePageIsApplied(t *testing.T) {
	session := newPageTestServer(t)
	session.post(t, "/api-access/tokens/create", url.Values{"name": {"temporary"}, "expires_days": {"7"}})
	tokens, _ := session.store.ListAPITokens(context.Background())
	if len(tokens) != 1 || tokens[0].ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expiry was not applied: %+v", tokens)
	}
}

func TestCallbackSecretCanBeRotatedAndClearedFromThePage(t *testing.T) {
	session := newPageTestServer(t)
	ctx := context.Background()

	page := session.get(t, "/api-access")
	if !strings.Contains(page.Body.String(), "未配置") {
		t.Fatal("an unconfigured callback secret must be visible as such")
	}

	rec := session.post(t, "/api-access/callback-secret/rotate", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("the secret must be rendered directly, not passed through a URL")
	}
	stored, err := session.store.CallbackSecret(ctx)
	if err != nil || stored == "" {
		t.Fatalf("rotate did not persist a secret: %q %v", stored, err)
	}
	if !strings.Contains(rec.Body.String(), stored) {
		t.Fatal("the rotated secret must be shown once")
	}

	reload := session.get(t, "/api-access")
	if strings.Contains(reload.Body.String(), stored) {
		t.Fatal("a reload must not reveal the callback secret again")
	}

	session.post(t, "/api-access/callback-secret/clear", url.Values{})
	if cleared, _ := session.store.CallbackSecret(ctx); cleared != "" {
		t.Fatalf("clear did not remove the secret: %q", cleared)
	}
}

func TestAPIAccessPageListsOnlyOptedInRules(t *testing.T) {
	session := newPageTestServer(t)
	ctx := context.Background()
	if err := session.store.UpsertRule(ctx, store.Rule{
		ID: "open-rule", SrcKind: "local", SrcLocalRoot: t.TempDir(),
		DstRemote: "gdrive", DstPath: "/Library", TransferMode: "move", APIEnabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := session.store.UpsertRule(ctx, store.Rule{
		ID: "closed-rule", SrcKind: "local", SrcLocalRoot: t.TempDir(),
		DstRemote: "gdrive", DstPath: "/Private", TransferMode: "copy",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	body := session.get(t, "/api-access").Body.String()
	if !strings.Contains(body, "open-rule") {
		t.Fatal("an opted-in rule must be listed")
	}
	if strings.Contains(body, "closed-rule") {
		t.Fatal("a rule that is closed to the API must not be listed")
	}
}

func extractToken(t *testing.T, body string) string {
	t.Helper()
	idx := strings.Index(body, "rst_")
	if idx < 0 {
		t.Fatalf("no token found in page body")
	}
	rest := body[idx:]
	end := strings.IndexAny(rest, "<\n\r \t")
	if end < 0 {
		t.Fatalf("token is not delimited in the page body")
	}
	return rest[:end]
}
