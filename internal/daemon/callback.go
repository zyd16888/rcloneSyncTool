package daemon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"115togd/internal/store"
)

// Callback delivery is a latency optimisation, never a source of truth. A
// client that never receives one still converges by polling, so a permanently
// failing endpoint must not affect the job outcome.
const (
	callbackMaxAttempts   = 5
	callbackTimeout       = 10 * time.Second
	callbackSignatureVer  = "v1"
	HeaderCallbackTime    = "X-Rclone-Syncd-Timestamp"
	HeaderCallbackSign    = "X-Rclone-Syncd-Signature"
	HeaderCallbackDeliver = "X-Rclone-Syncd-Delivery"
)

// callbackPayload carries only identifiers and counters. It never includes the
// token, the signing secret, absolute paths or the request snapshot.
type callbackPayload struct {
	DeliveryID     string `json:"delivery_id"`
	Event          string `json:"event"`
	JobID          string `json:"job_id"`
	ExternalID     string `json:"external_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Status         string `json:"status"`
	BlockReason    string `json:"block_reason,omitempty"`
	BytesDone      int64  `json:"bytes_done"`
	FilesDone      int    `json:"files_done"`
	FilesFailed    int    `json:"files_failed"`
	ErrorMessage   string `json:"error_message,omitempty"`
	OccurredAt     string `json:"occurred_at"`
}

// StartCallbackQueue retries pending deliveries, including ones interrupted by
// a restart. Delivery state lives in the database precisely so a crash between
// finishing a job and notifying the client is recoverable.
func (s *Supervisor) StartCallbackQueue(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.drainCallbackQueue(ctx)
		}
	}
}

func (s *Supervisor) drainCallbackQueue(ctx context.Context) {
	jobs, err := s.st.DueCallbackJobs(ctx, time.Now(), callbackMaxAttempts, 20)
	if err != nil {
		log.Printf("callback queue: list jobs: %v", err)
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		s.deliverCallback(ctx, job)
	}
}

func (s *Supervisor) deliverCallback(ctx context.Context, job store.TransferJob) {
	secret, err := s.st.CallbackSecret(ctx)
	if err != nil || secret == "" {
		// Without a shared secret the receiver cannot verify anything, so an
		// unsigned request would be worse than no request at all.
		_ = s.st.RecordCallbackAttempt(ctx, job.JobID, "unconfigured",
			job.CallbackAttempts, time.Now().Add(30*time.Second))
		return
	}
	claimed, ok, err := s.st.ClaimCallback(ctx, job.JobID, callbackMaxAttempts)
	if err != nil || !ok {
		return
	}
	job = claimed
	if allowed, _, _ := s.st.Setting(ctx, "callback_allowed_hosts"); strings.TrimSpace(allowed) != "" {
		u, err := url.Parse(job.CallbackURL)
		accepted := false
		if err == nil {
			for _, host := range strings.FieldsFunc(allowed, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
				if strings.EqualFold(strings.TrimSpace(host), u.Host) || strings.EqualFold(strings.TrimSpace(host), u.Hostname()) {
					accepted = true
				}
			}
		}
		if !accepted {
			_ = s.st.RecordCallbackAttempt(ctx, job.JobID, "invalid_url", job.CallbackAttempts, time.Time{})
			return
		}
	}
	counts, err := s.st.TransferJobFileCounts(ctx, job.JobID)
	if err != nil {
		log.Printf("callback %s: counts: %v", job.JobID, err)
		return
	}
	payload := callbackPayload{
		DeliveryID:     newID(),
		Event:          "transfer_job.terminal",
		JobID:          job.JobID,
		ExternalID:     job.ExternalID,
		IdempotencyKey: job.IdempotencyKey,
		Status:         job.Status,
		BlockReason:    job.BlockReason,
		BytesDone:      job.BytesDone,
		FilesDone:      counts.Done,
		FilesFailed:    counts.Failed,
		ErrorMessage:   job.Error,
		OccurredAt:     time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("callback %s: encode: %v", job.JobID, err)
		return
	}

	attempts := job.CallbackAttempts
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.CallbackURL, bytes.NewReader(body))
	if err != nil {
		_ = s.st.RecordCallbackAttempt(ctx, job.JobID, "invalid_url", callbackMaxAttempts, time.Time{})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderCallbackTime, timestamp)
	req.Header.Set(HeaderCallbackDeliver, payload.DeliveryID)
	req.Header.Set(HeaderCallbackSign, callbackSignatureVer+"="+SignCallback(secret, timestamp, body))

	client := &http.Client{Timeout: callbackTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
	}
	delivered := err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300
	if delivered {
		_ = s.st.RecordCallbackAttempt(ctx, job.JobID, "delivered", attempts, time.Time{})
		return
	}
	state := "failed"
	if attempts >= callbackMaxAttempts {
		// Give up quietly: polling remains the reliable path and the job
		// outcome is already final and correct.
		state = "abandoned"
		log.Printf("callback %s: giving up after %d attempts", job.JobID, attempts)
	}
	_ = s.st.RecordCallbackAttempt(ctx, job.JobID, state, attempts, callbackBackoff(attempts))
}

// callbackBackoff spreads retries from seconds to minutes so a receiver that is
// restarting is not hammered while it comes back up.
func callbackBackoff(attempts int) time.Time {
	seconds := 5 << uint(maxInt(0, attempts-1))
	if seconds > 300 {
		seconds = 300
	}
	return time.Now().Add(time.Duration(seconds) * time.Second)
}

// SignCallback produces the hex HMAC-SHA256 over "<timestamp>.<body>". Binding
// the timestamp into the signature is what makes a captured request unusable
// once the receiver clock skew window passes.
func SignCallback(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
