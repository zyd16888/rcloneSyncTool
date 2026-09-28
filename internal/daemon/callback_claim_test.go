package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentCallbackDeliveryClaimsOnlyOnce(t *testing.T) {
	st := newCallbackStore(t)
	ctx := context.Background()
	_ = st.SetCallbackSecret(ctx, testSecret)
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	job := seedTerminalJob(t, st, server.URL)
	supervisor := NewSupervisor(st)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); supervisor.deliverCallback(ctx, job) }()
	}
	wg.Wait()
	if received.Load() != 1 {
		t.Fatalf("duplicate callbacks: %d", received.Load())
	}
}
func TestUnconfiguredCallbackDoesNotConsumeRetries(t *testing.T) {
	st := newCallbackStore(t)
	job := seedTerminalJob(t, st, "http://127.0.0.1:1/callback")
	s := NewSupervisor(st)
	for i := 0; i < 6; i++ {
		s.deliverCallback(context.Background(), job)
	}
	current, _, _ := st.GetTransferJob(context.Background(), job.JobID)
	if current.CallbackAttempts != 0 {
		t.Fatalf("unconfigured callback exhausted attempts: %d", current.CallbackAttempts)
	}
}
