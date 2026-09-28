package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"115togd/internal/store"
)

func TestRulesFilterUsesActualTasksEvenWhenPaused(t *testing.T) {
	session := newPageTestServer(t)
	ctx := context.Background()
	for _, id := range []string{"active-task", "enabled-idle"} {
		if err := session.store.UpsertRule(ctx, store.Rule{ID: id, SrcKind: "local", SrcLocalRoot: t.TempDir(), DstRemote: "mock", DstPath: "/library", Enabled: id == "enabled-idle"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.store.CreateTransferJob(ctx, store.TransferJob{JobID: "running", RuleID: "active-task", TransferMode: "copy"}, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = session.store.StartTransferJob(ctx, "running", 55720)
	response := session.get(t, "/rules?runtime=running")
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "active-task") || strings.Contains(body, "enabled-idle") {
		t.Fatalf("enabled state was confused with actual task: %s", body)
	}
}
func TestSettingsValidationIsAtomic(t *testing.T) {
	session := newPageTestServer(t)
	ctx := context.Background()
	_ = session.store.SetSetting(ctx, "global_max_jobs", "2")
	response := session.post(t, "/settings/save", url.Values{"global_max_jobs": {"8"}, "metrics_interval_ms": {"0"}})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid interval accepted: %d", response.Code)
	}
	value, _, _ := session.store.Setting(ctx, "global_max_jobs")
	if value != "2" {
		t.Fatal("invalid settings partially persisted")
	}
}
