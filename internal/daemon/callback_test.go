package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"115togd/internal/store"
)

const testSecret = "callback-secret-fixture-value"

func newCallbackStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "callback.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	if err := st.UpsertRule(ctx, store.Rule{
		ID: "local-to-gdrive", SrcKind: "local", SrcLocalRoot: t.TempDir(),
		DstRemote: "gdrive", DstPath: "/Library", TransferMode: "move", APIEnabled: true,
	}); err != nil {
		t.Fatalf("upsert rule: %v", err)
	}
	return st
}

func seedTerminalJob(t *testing.T, st *store.Store, callbackURL string) store.TransferJob {
	t.Helper()
	ctx := context.Background()
	job := store.TransferJob{
		JobID:          "job-1",
		RuleID:         "local-to-gdrive",
		TransferMode:   "move",
		ExternalID:     "node-run-1",
		IdempotencyKey: "run-1:upload:1",
		Fingerprint:    "fp",
		RequestJSON:    `{"operation":"move"}`,
		CallbackURL:    callbackURL,
		LogPath:        filepath.Join(t.TempDir(), "job.log"),
	}
	if err := st.CreateTransferJob(ctx, job, nil); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := st.FinishTransferJob(ctx, job.JobID, store.TransferStatusDone, 4096, 100, "", nil); err != nil {
		t.Fatalf("finish job: %v", err)
	}
	stored, _, err := st.GetTransferJob(ctx, job.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return stored
}

func TestSignCallbackBindsTimestampAndBody(t *testing.T) {
	body := []byte(`{"job_id":"job-1"}`)
	signature := SignCallback(testSecret, "1700000000", body)
	if signature == "" || len(signature) != 64 {
		t.Fatalf("unexpected signature: %q", signature)
	}
	if SignCallback(testSecret, "1700000001", body) == signature {
		t.Fatal("a different timestamp must produce a different signature")
	}
	if SignCallback(testSecret, "1700000000", []byte(`{"job_id":"job-2"}`)) == signature {
		t.Fatal("a different body must produce a different signature")
	}
	if SignCallback("other-secret", "1700000000", body) == signature {
		t.Fatal("a different secret must produce a different signature")
	}
}

func TestCallbackBackoffGrowsAndCaps(t *testing.T) {
	first := time.Until(callbackBackoff(1)).Round(time.Second)
	second := time.Until(callbackBackoff(2)).Round(time.Second)
	capped := time.Until(callbackBackoff(20)).Round(time.Second)
	if second <= first {
		t.Fatalf("backoff must grow: %v then %v", first, second)
	}
	if capped > 301*time.Second {
		t.Fatalf("backoff must be capped, got %v", capped)
	}
}

func TestDeliverCallbackSignsAndMarksDelivered(t *testing.T) {
	st := newCallbackStore(t)
	ctx := context.Background()
	if err := st.SetCallbackSecret(ctx, testSecret); err != nil {
		t.Fatalf("set secret: %v", err)
	}

	var received atomic.Int32
	var gotBody []byte
	var gotSignature, gotTimestamp, gotDelivery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		gotBody, _ = io.ReadAll(r.Body)
		gotSignature = r.Header.Get(HeaderCallbackSign)
		gotTimestamp = r.Header.Get(HeaderCallbackTime)
		gotDelivery = r.Header.Get(HeaderCallbackDeliver)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	job := seedTerminalJob(t, st, server.URL+"/api/workflows/callbacks/transfer/rclone-sync/gdrive-main")
	NewSupervisor(st).deliverCallback(ctx, job)

	if received.Load() != 1 {
		t.Fatalf("expected one delivery, got %d", received.Load())
	}
	if gotDelivery == "" || gotTimestamp == "" {
		t.Fatal("delivery id and timestamp headers are required")
	}
	expected := "v1=" + SignCallback(testSecret, gotTimestamp, gotBody)
	if gotSignature != expected {
		t.Fatalf("signature mismatch:\n got %s\nwant %s", gotSignature, expected)
	}

	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload["job_id"] != "job-1" || payload["external_id"] != "node-run-1" {
		t.Fatalf("unexpected payload: %s", gotBody)
	}
	if payload["status"] != store.TransferStatusDone {
		t.Fatalf("unexpected status: %v", payload["status"])
	}
	if strings.Contains(string(gotBody), testSecret) {
		t.Fatal("the signing secret must never appear in the payload")
	}

	updated, _, err := st.GetTransferJob(ctx, job.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if updated.CallbackState != "delivered" {
		t.Fatalf("expected delivered, got %q", updated.CallbackState)
	}
	// A delivered job must not be picked up again.
	due, err := st.DueCallbackJobs(ctx, time.Now(), callbackMaxAttempts, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("delivered job must leave the queue: %d %v", len(due), err)
	}
}

func TestFailedDeliveryRetriesAndNeverChangesTheJobOutcome(t *testing.T) {
	st := newCallbackStore(t)
	ctx := context.Background()
	_ = st.SetCallbackSecret(ctx, testSecret)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	job := seedTerminalJob(t, st, server.URL+"/callback")
	supervisor := NewSupervisor(st)
	supervisor.deliverCallback(ctx, job)

	updated, _, _ := st.GetTransferJob(ctx, job.JobID)
	if updated.CallbackState != "failed" || updated.CallbackAttempts != 1 {
		t.Fatalf("unexpected callback state: %q attempts=%d", updated.CallbackState, updated.CallbackAttempts)
	}
	// The transfer itself stays successful: notification is not the outcome.
	if updated.Status != store.TransferStatusDone {
		t.Fatalf("a failed callback must not change the job status, got %q", updated.Status)
	}
	if updated.CallbackNextAt.IsZero() {
		t.Fatal("a failed delivery must schedule a retry")
	}
}

func TestDeliveryIsAbandonedAfterTheAttemptLimit(t *testing.T) {
	st := newCallbackStore(t)
	ctx := context.Background()
	_ = st.SetCallbackSecret(ctx, testSecret)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	job := seedTerminalJob(t, st, server.URL+"/callback")
	supervisor := NewSupervisor(st)
	for i := 0; i < callbackMaxAttempts; i++ {
		current, _, _ := st.GetTransferJob(ctx, job.JobID)
		supervisor.deliverCallback(ctx, current)
	}
	updated, _, _ := st.GetTransferJob(ctx, job.JobID)
	if updated.CallbackState != "abandoned" {
		t.Fatalf("expected abandoned, got %q", updated.CallbackState)
	}
	due, _ := st.DueCallbackJobs(ctx, time.Now(), callbackMaxAttempts, 10)
	if len(due) != 0 {
		t.Fatal("an abandoned delivery must stop consuming the queue")
	}
}

func TestCallbackIsSkippedWithoutASharedSecret(t *testing.T) {
	st := newCallbackStore(t)
	ctx := context.Background()

	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	job := seedTerminalJob(t, st, server.URL+"/callback")
	NewSupervisor(st).deliverCallback(ctx, job)

	if received.Load() != 0 {
		t.Fatal("an unsigned callback must never be sent")
	}
	updated, _, _ := st.GetTransferJob(ctx, job.JobID)
	if updated.CallbackState != "unconfigured" {
		t.Fatalf("expected unconfigured, got %q", updated.CallbackState)
	}
}

func TestDueCallbackJobsOnlyListsTerminalJobsWithAUrl(t *testing.T) {
	st := newCallbackStore(t)
	ctx := context.Background()

	// Terminal but with no callback configured.
	silent := store.TransferJob{
		JobID: "job-silent", RuleID: "local-to-gdrive", TransferMode: "move",
		IdempotencyKey: "k1", LogPath: "x",
	}
	if err := st.CreateTransferJob(ctx, silent, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = st.FinishTransferJob(ctx, silent.JobID, store.TransferStatusDone, 0, 0, "", nil)

	// Configured but still pending, so nothing to report yet.
	pending := store.TransferJob{
		JobID: "job-pending", RuleID: "local-to-gdrive", TransferMode: "move",
		IdempotencyKey: "k2", CallbackURL: "https://example.invalid/cb", LogPath: "x",
	}
	if err := st.CreateTransferJob(ctx, pending, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	seedTerminalJob(t, st, "https://example.invalid/cb")

	due, err := st.DueCallbackJobs(ctx, time.Now(), callbackMaxAttempts, 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 1 || due[0].JobID != "job-1" {
		t.Fatalf("unexpected queue contents: %+v", due)
	}
}

func TestScheduledRetryIsNotDueYet(t *testing.T) {
	st := newCallbackStore(t)
	ctx := context.Background()
	job := seedTerminalJob(t, st, "https://example.invalid/cb")
	if err := st.RecordCallbackAttempt(ctx, job.JobID, "failed", 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("record: %v", err)
	}
	due, _ := st.DueCallbackJobs(ctx, time.Now(), callbackMaxAttempts, 10)
	if len(due) != 0 {
		t.Fatal("a scheduled retry must wait for its backoff")
	}
	later, _ := st.DueCallbackJobs(ctx, time.Now().Add(2*time.Hour), callbackMaxAttempts, 10)
	if len(later) != 1 {
		t.Fatal("the retry must become due once the backoff passes")
	}
}
