package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func TestCreateAPITokenStoresOnlyHash(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	token, plaintext, err := st.CreateAPIToken(ctx, "workflow", time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if plaintext == "" || len(plaintext) < 20 {
		t.Fatalf("unexpected plaintext: %q", plaintext)
	}
	if token.Hash == plaintext {
		t.Fatal("plaintext token was persisted")
	}
	if token.Hash != HashAPIToken(plaintext) {
		t.Fatal("stored hash does not match the plaintext")
	}

	raw, ok, err := st.Setting(ctx, apiTokensSettingKey)
	if err != nil || !ok {
		t.Fatalf("read setting: ok=%v err=%v", ok, err)
	}
	if contains(raw, plaintext) {
		t.Fatal("plaintext token leaked into the settings row")
	}
}

func TestAuthenticateAPIToken(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	token, plaintext, err := st.CreateAPIToken(ctx, "workflow", time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, ok, err := st.AuthenticateAPIToken(ctx, plaintext)
	if err != nil || !ok {
		t.Fatalf("expected authentication to succeed: ok=%v err=%v", ok, err)
	}
	if got.ID != token.ID {
		t.Fatalf("wrong token: %s != %s", got.ID, token.ID)
	}

	if _, ok, _ := st.AuthenticateAPIToken(ctx, plaintext+"x"); ok {
		t.Fatal("a wrong token authenticated")
	}
	if _, ok, _ := st.AuthenticateAPIToken(ctx, ""); ok {
		t.Fatal("an empty token authenticated")
	}
}

func TestDisabledAndExpiredTokensAreRejected(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	disabled, disabledPlain, err := st.CreateAPIToken(ctx, "disabled", time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.SetAPITokenEnabled(ctx, disabled.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok, _ := st.AuthenticateAPIToken(ctx, disabledPlain); ok {
		t.Fatal("a disabled token authenticated")
	}
	if err := st.SetAPITokenEnabled(ctx, disabled.ID, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if _, ok, _ := st.AuthenticateAPIToken(ctx, disabledPlain); !ok {
		t.Fatal("re-enabling a token did not restore access")
	}

	_, expiredPlain, err := st.CreateAPIToken(ctx, "expired", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("create expired: %v", err)
	}
	if _, ok, _ := st.AuthenticateAPIToken(ctx, expiredPlain); ok {
		t.Fatal("an expired token authenticated")
	}
}

func TestDeleteAPIToken(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	token, plaintext, err := st.CreateAPIToken(ctx, "temp", time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.DeleteAPIToken(ctx, token.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := st.AuthenticateAPIToken(ctx, plaintext); ok {
		t.Fatal("a deleted token authenticated")
	}
	if err := st.DeleteAPIToken(ctx, token.ID); err != ErrAPITokenNotFound {
		t.Fatalf("expected ErrAPITokenNotFound, got %v", err)
	}
}

func TestRuleAPIOperationAllowList(t *testing.T) {
	rule := Rule{
		ID:           "r1",
		SrcKind:      "local",
		SrcLocalRoot: "/data/watch",
		DstRemote:    "gdrive",
		DstPath:      "/Library",
		TransferMode: "move",
	}
	if err := rule.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !rule.AllowsAPIOperation("move") {
		t.Fatal("an empty allow-list must fall back to the rule transfer mode")
	}
	if rule.AllowsAPIOperation("copy") {
		t.Fatal("an empty allow-list must not widen the rule behaviour")
	}

	rule.APIAllowedOperations = " MOVE, copy ,move "
	if err := rule.Normalize(); err != nil {
		t.Fatalf("normalize allow-list: %v", err)
	}
	if rule.APIAllowedOperations != "move,copy" {
		t.Fatalf("unexpected normalized allow-list: %q", rule.APIAllowedOperations)
	}

	rule.APIAllowedOperations = "sync"
	if err := rule.Normalize(); err == nil {
		t.Fatal("an unsupported operation must be rejected")
	}
}

func TestRuleAPIFieldsRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	rule := Rule{
		ID:                   "api-rule",
		SrcKind:              "local",
		SrcLocalRoot:         "/data/watch",
		DstRemote:            "gdrive",
		DstPath:              "/Library",
		TransferMode:         "move",
		APIEnabled:           true,
		APIAllowedOperations: "copy,move",
	}
	if err := st.UpsertRule(ctx, rule); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, ok, err := st.GetRule(ctx, "api-rule")
	if err != nil || !ok {
		t.Fatalf("get rule: ok=%v err=%v", ok, err)
	}
	if !got.APIEnabled || got.APIAllowedOperations != "copy,move" {
		t.Fatalf("api fields did not round-trip: %+v", got)
	}

	listed, err := st.ListRules(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list rules: %d %v", len(listed), err)
	}
	if !listed[0].APIEnabled {
		t.Fatal("ListRules dropped the api_enabled flag")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
