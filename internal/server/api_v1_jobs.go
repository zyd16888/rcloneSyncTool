package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"115togd/internal/daemon"
	"115togd/internal/store"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"
	maxIdempotencyKeyLen = 256
	maxExternalIDLen     = 256
	defaultFilePageSize  = 200
	maxFilePageSize      = 500
	defaultLogPageBytes  = 64 * 1024
	maxLogPageBytes      = 256 * 1024
	maxInlineFiles       = 50
)

type transferJobRequest struct {
	ExternalID         string `json:"external_id"`
	RuleID             string `json:"rule_id"`
	Operation          string `json:"operation"`
	SourceSubpath      string `json:"source_subpath"`
	DestinationSubpath string `json:"destination_subpath"`
	Files              []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	} `json:"files"`
	Metadata map[string]any `json:"metadata"`
	Callback *struct {
		URL string `json:"url"`
	} `json:"callback"`
}

// createTransferJob accepts one transfer request. It is idempotent on
// Idempotency-Key: replaying the same request returns the same job, and
// reusing the key with a different request is a conflict rather than a second
// transfer. That is what lets a client retry a timed-out submit safely.
func (s *Server) createTransferJob(c *gin.Context) {
	key := strings.TrimSpace(c.GetHeader(idempotencyKeyHeader))
	if key == "" || len(key) > maxIdempotencyKeyLen {
		apiFail(c, http.StatusBadRequest, "invalid_request",
			"Idempotency-Key header is required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, apiMaxRequestBytes+1))
	if err != nil {
		apiFail(c, http.StatusBadRequest, "invalid_request", "cannot read request body")
		return
	}
	if len(body) > apiMaxRequestBytes {
		apiFail(c, http.StatusRequestEntityTooLarge, "payload_too_large",
			"request body exceeds the advertised limit")
		return
	}
	var req transferJobRequest
	if err := json.Unmarshal(body, &req); err != nil {
		apiFail(c, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
		return
	}

	ctx := c.Request.Context()
	fingerprint := requestFingerprint(body)

	// An existing key short-circuits before any validation so a replay behaves
	// identically even if the rule was closed to the API in the meantime.
	if existing, ok, err := s.st.GetTransferJobByIdempotencyKey(ctx, key); err != nil {
		apiFail(c, http.StatusInternalServerError, "internal_error", "idempotency lookup failed")
		return
	} else if ok {
		if existing.Fingerprint != fingerprint {
			apiFail(c, http.StatusConflict, "idempotency_key_conflict",
				"this Idempotency-Key was used with a different request")
			return
		}
		c.JSON(http.StatusOK, gin.H{"idempotent": true, "job": s.transferJobPayload(ctx, existing)})
		return
	}

	spec, rule, callbackURL, apiErr := s.validateTransferRequest(ctx, &req)
	if apiErr != nil {
		apiFail(c, apiErr.status, apiErr.code, apiErr.message)
		return
	}

	if req.ExternalID != "" {
		if existing, ok, err := s.st.GetTransferJobByExternalID(ctx, req.ExternalID); err == nil && ok {
			if existing.IdempotencyKey != key {
				apiFail(c, http.StatusConflict, "external_id_conflict",
					"external_id already belongs to another job")
				return
			}
		}
	}

	encodedSpec, err := daemon.EncodeTransferSpec(spec)
	if err != nil {
		apiFail(c, http.StatusInternalServerError, "internal_error", "cannot encode request")
		return
	}

	settings, err := s.st.RuntimeSettings(ctx)
	if err != nil {
		apiFail(c, http.StatusInternalServerError, "internal_error", "cannot load settings")
		return
	}
	jobID := newID()
	groupKey := ""
	if rule.GroupByDirectory && spec.SourceSubpath != "" {
		groupKey = "dir:" + spec.SourceSubpath
	}
	job := store.TransferJob{
		JobID:          jobID,
		RuleID:         rule.ID,
		TransferMode:   spec.Operation,
		ExternalID:     req.ExternalID,
		IdempotencyKey: key,
		Fingerprint:    fingerprint,
		RequestJSON:    encodedSpec,
		CallbackURL:    callbackURL,
		LogPath:        filepath.Join(settings.LogDir, rule.ID, jobID+".log"),
		QuotaGroup:     rule.LimitGroup,
		GroupKey:       groupKey,
	}
	if err := s.st.CreateTransferJob(ctx, job, spec.Files); err != nil {
		// A concurrent submit with the same key lost the race; return the
		// winner so both callers observe exactly one external job.
		if existing, ok, lookupErr := s.st.GetTransferJobByIdempotencyKey(ctx, key); lookupErr == nil && ok {
			if existing.Fingerprint != fingerprint {
				apiFail(c, http.StatusConflict, "idempotency_key_conflict", "this Idempotency-Key was used with a different request")
				return
			}
			c.JSON(http.StatusOK, gin.H{"idempotent": true, "job": s.transferJobPayload(ctx, existing)})
			return
		}
		apiFail(c, http.StatusConflict, "job_create_failed", "cannot create transfer job")
		return
	}

	created, _, err := s.st.GetTransferJob(ctx, jobID)
	if err != nil {
		apiFail(c, http.StatusInternalServerError, "internal_error", "cannot read created job")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"idempotent": false, "job": s.transferJobPayload(ctx, created)})
}

type apiFailure struct {
	status  int
	code    string
	message string
}

func fail(status int, code, message string) *apiFailure {
	return &apiFailure{status: status, code: code, message: message}
}

// validateTransferRequest turns an untrusted request into a spec the daemon can
// execute. Every path is normalized against the rule root here, and the rule
// itself decides which operations the API may request.
func (s *Server) validateTransferRequest(
	ctx context.Context,
	req *transferJobRequest,
) (daemon.TransferSpec, store.Rule, string, *apiFailure) {
	var spec daemon.TransferSpec

	if len(req.ExternalID) > maxExternalIDLen {
		return spec, store.Rule{}, "", fail(http.StatusBadRequest, "invalid_request",
			"external_id is too long")
	}
	ruleID := strings.TrimSpace(req.RuleID)
	if ruleID == "" {
		return spec, store.Rule{}, "", fail(http.StatusBadRequest, "invalid_request",
			"rule_id is required")
	}
	rule, ok, err := s.st.GetRule(ctx, ruleID)
	if err != nil {
		return spec, store.Rule{}, "", fail(http.StatusInternalServerError, "internal_error",
			"cannot load rule")
	}
	// A rule that is not opted in must be indistinguishable from one that does
	// not exist, so the API never reveals the private rule inventory.
	if !ok || !rule.APIEnabled || rule.IsManual {
		return spec, store.Rule{}, "", fail(http.StatusNotFound, "rule_not_found",
			"rule is not available to the API")
	}

	operation := strings.TrimSpace(strings.ToLower(req.Operation))
	if operation == "" {
		operation = rule.TransferMode
	}
	if operation != "copy" && operation != "move" {
		return spec, store.Rule{}, "", fail(http.StatusBadRequest, "unsupported_operation",
			"operation must be copy or move")
	}
	if !rule.AllowsAPIOperation(operation) {
		return spec, store.Rule{}, "", fail(http.StatusBadRequest, "operation_not_allowed",
			"rule does not allow operation "+operation)
	}

	sourceSubpath, err := normalizeSubpath(req.SourceSubpath)
	if err != nil {
		return spec, store.Rule{}, "", fail(http.StatusBadRequest, "path_escape",
			"source_subpath must stay inside the rule root")
	}
	destSubpath, err := normalizeSubpath(req.DestinationSubpath)
	if err != nil {
		return spec, store.Rule{}, "", fail(http.StatusBadRequest, "path_escape",
			"destination_subpath must stay inside the rule root")
	}
	if rule.SrcKind == "local" {
		if _, err := resolveLocalSource(rule.SrcLocalRoot, sourceSubpath); err != nil {
			return spec, store.Rule{}, "", fail(http.StatusBadRequest, "path_escape",
				"source_subpath resolves outside the rule root")
		}
	}

	if len(req.Files) > apiMaxFilesPerJob {
		return spec, store.Rule{}, "", fail(http.StatusBadRequest, "file_list_too_large",
			"file list exceeds the advertised limit")
	}
	seen := make(map[string]bool, len(req.Files))
	files := make([]store.TransferJobFile, 0, len(req.Files))
	for _, item := range req.Files {
		path, err := normalizeSubpath(item.Path)
		if err != nil || path == "" {
			return spec, store.Rule{}, "", fail(http.StatusBadRequest, "path_escape",
				"file paths must be relative to the source directory")
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		if rule.SrcKind == "local" {
			if _, err := resolveLocalSource(rule.SrcLocalRoot, filepath.Join(sourceSubpath, path)); err != nil {
				return spec, store.Rule{}, "", fail(http.StatusBadRequest, "path_escape", "file resolves outside the rule root")
			}
		}
		size := item.Size
		if size < 0 {
			size = 0
		}
		sourcePath := filepath.ToSlash(filepath.Join(sourceSubpath, path))
		key, _ := store.FileGroupKey(rule, sourcePath)
		files = append(files, store.TransferJobFile{Path: path, Size: size, SourcePath: sourcePath, GroupKey: key})
	}

	callbackURL := ""
	if req.Callback != nil {
		callbackURL, err = validCallbackURL(req.Callback.URL)
		if err != nil {
			return spec, store.Rule{}, "", fail(http.StatusBadRequest, "invalid_request",
				"callback url is invalid")
		}
	}

	spec = daemon.TransferSpec{
		Operation:          operation,
		SourceSubpath:      sourceSubpath,
		DestinationSubpath: destSubpath,
		Files:              files,
		RuleSnapshot:       &rule,
	}
	return spec, rule, callbackURL, nil
}

func requestFingerprint(body []byte) string {
	// Hash the canonical form so key ordering and whitespace do not turn a
	// replay into a false conflict.
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:])
	}
	canonical, err := json.Marshal(parsed)
	if err != nil {
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func (s *Server) getTransferJob(c *gin.Context) {
	ctx := c.Request.Context()
	job, ok, err := s.st.GetTransferJob(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil {
		apiFail(c, http.StatusInternalServerError, "internal_error", "cannot read job")
		return
	}
	if !ok || job.Origin != store.OriginAPI {
		apiFail(c, http.StatusNotFound, "job_not_found", "transfer job not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": s.transferJobPayload(ctx, job)})
}

func (s *Server) getTransferJobByExternalID(c *gin.Context) {
	ctx := c.Request.Context()
	job, ok, err := s.st.GetTransferJobByExternalID(ctx, strings.TrimSpace(c.Param("external_id")))
	if err != nil {
		apiFail(c, http.StatusInternalServerError, "internal_error", "cannot read job")
		return
	}
	if !ok {
		apiFail(c, http.StatusNotFound, "job_not_found", "transfer job not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": s.transferJobPayload(ctx, job)})
}

func (s *Server) listTransferJobFiles(c *gin.Context) {
	ctx := c.Request.Context()
	job, ok, err := s.st.GetTransferJob(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil || !ok || job.Origin != store.OriginAPI {
		apiFail(c, http.StatusNotFound, "job_not_found", "transfer job not found")
		return
	}
	after, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		apiFail(c, http.StatusBadRequest, "invalid_request", "invalid cursor")
		return
	}
	limit := clampInt(c.Query("limit"), defaultFilePageSize, 1, maxFilePageSize)
	files, err := s.st.ListTransferJobFiles(ctx, job.JobID, after, limit)
	if err != nil {
		apiFail(c, http.StatusInternalServerError, "internal_error", "cannot read files")
		return
	}
	items := make([]gin.H, 0, len(files))
	for _, f := range files {
		items = append(items, gin.H{
			"path":       f.Path,
			"size":       f.Size,
			"state":      f.State,
			"last_error": f.LastError,
		})
	}
	next := ""
	if len(files) == limit {
		next = encodeCursor(files[len(files)-1].Path)
	}
	counts, _ := s.st.TransferJobFileCounts(ctx, job.JobID)
	c.JSON(http.StatusOK, gin.H{
		"job_id":      job.JobID,
		"files":       items,
		"next_cursor": next,
		"totals": gin.H{
			"files_total":  counts.Total,
			"files_done":   counts.Done,
			"files_failed": counts.Failed,
		},
	})
}

// getTransferJobLogs streams the rclone log by byte offset so a client can
// follow a long transfer without ever loading the whole file.
func (s *Server) getTransferJobLogs(c *gin.Context) {
	ctx := c.Request.Context()
	job, ok, err := s.st.GetTransferJob(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil || !ok || job.Origin != store.OriginAPI {
		apiFail(c, http.StatusNotFound, "job_not_found", "transfer job not found")
		return
	}
	rawCursor, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		apiFail(c, http.StatusBadRequest, "invalid_request", "invalid cursor")
		return
	}
	offset := int64(0)
	if rawCursor != "" {
		parsed, parseErr := strconv.ParseInt(rawCursor, 10, 64)
		if parseErr != nil || parsed < 0 {
			apiFail(c, http.StatusBadRequest, "invalid_request", "invalid cursor")
			return
		}
		offset = parsed
	}
	limit := int64(clampInt(c.Query("limit"), defaultLogPageBytes, 1024, maxLogPageBytes))

	file, err := os.Open(job.LogPath)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"job_id": job.JobID, "lines": []string{},
			"next_cursor": encodeCursor("0"), "eof": job.Terminal(),
		})
		return
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		apiFail(c, http.StatusBadRequest, "invalid_request", "cursor is past the end of the log")
		return
	}
	buf := make([]byte, limit)
	read, err := file.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		apiFail(c, http.StatusInternalServerError, "internal_error", "cannot read log")
		return
	}
	chunk := string(buf[:read])
	// Keep the cursor on a line boundary so a caller never sees half a line.
	if read == int(limit) {
		if idx := strings.LastIndexByte(chunk, '\n'); idx >= 0 {
			chunk = chunk[:idx+1]
			read = idx + 1
		}
	}
	lines := make([]string, 0, 32)
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		lines = append(lines, redactLogLine(line))
	}
	c.JSON(http.StatusOK, gin.H{
		"job_id":      job.JobID,
		"lines":       lines,
		"next_cursor": encodeCursor(strconv.FormatInt(offset+int64(read), 10)),
		"eof":         job.Terminal() && read == 0,
	})
}

func (s *Server) cancelTransferJob(c *gin.Context) {
	ctx := c.Request.Context()
	job, ok, err := s.st.GetTransferJob(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil || !ok || job.Origin != store.OriginAPI {
		apiFail(c, http.StatusNotFound, "job_not_found", "transfer job not found")
		return
	}
	// Cancelling an already terminal job is a no-op, not an error: a client
	// retrying a cancel must converge instead of failing.
	if job.Terminal() {
		c.JSON(http.StatusOK, gin.H{"job": s.transferJobPayload(ctx, job)})
		return
	}
	switch job.Status {
	case store.TransferStatusPending, store.TransferStatusBlocked:
		if cancelled, err := s.st.CancelWaitingTask(ctx, job.JobID); err != nil || !cancelled {
			apiFail(c, http.StatusInternalServerError, "internal_error", "cannot cancel job")
			return
		}
	case store.TransferStatusRunning:
		if s.supervisor == nil || !s.supervisor.TerminateJob(job.JobID) {
			apiFail(c, http.StatusConflict, "job_not_cancellable",
				"job is running but not present in the local registry")
			return
		}
	}
	updated, _, _ := s.st.GetTransferJob(ctx, job.JobID)
	c.JSON(http.StatusOK, gin.H{"job": s.transferJobPayload(ctx, updated)})
}

// retryTransferJob creates a NEW job from a terminal one. It deliberately does
// not resurrect the original: a retry is a distinct external side effect and
// must carry its own idempotency key.
func (s *Server) retryTransferJob(c *gin.Context) {
	ctx := c.Request.Context()
	key := strings.TrimSpace(c.GetHeader(idempotencyKeyHeader))
	if key == "" || len(key) > maxIdempotencyKeyLen {
		apiFail(c, http.StatusBadRequest, "invalid_request",
			"Idempotency-Key header is required for a retry")
		return
	}
	source, ok, err := s.st.GetTransferJob(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil || !ok || source.Origin != store.OriginAPI {
		apiFail(c, http.StatusNotFound, "job_not_found", "transfer job not found")
		return
	}
	if !source.Terminal() {
		apiFail(c, http.StatusConflict, "job_not_retryable",
			"only a terminal job can be retried")
		return
	}
	if key == source.IdempotencyKey {
		apiFail(c, http.StatusConflict, "idempotency_key_conflict", "a retry requires a new Idempotency-Key")
		return
	}
	if existing, found, _ := s.st.GetTransferJobByIdempotencyKey(ctx, key); found {
		if existing.Fingerprint != source.Fingerprint || existing.RetryOf != source.JobID {
			apiFail(c, http.StatusConflict, "idempotency_key_conflict",
				"this Idempotency-Key was used with a different request")
			return
		}
		c.JSON(http.StatusOK, gin.H{"idempotent": true, "job": s.transferJobPayload(ctx, existing)})
		return
	}

	created, err := daemon.RetryTask(ctx, s.st, source, newID(), key)
	if err != nil {
		if existing, found, _ := s.st.GetTransferJobByIdempotencyKey(ctx, key); found && existing.RetryOf == source.JobID {
			c.JSON(http.StatusOK, gin.H{"idempotent": true, "job": s.transferJobPayload(ctx, existing)})
			return
		}
		apiFail(c, http.StatusConflict, "job_not_retryable", err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"idempotent": false,
		"retry_of":   source.JobID,
		"job":        s.transferJobPayload(ctx, created),
	})
}

// ── payloads ────────────────────────────────────────────────────────────────

func (s *Server) transferJobPayload(ctx context.Context, job store.TransferJob) gin.H {
	payload := gin.H{
		"job_id":          job.JobID,
		"rule_id":         job.RuleID,
		"external_id":     job.ExternalID,
		"idempotency_key": job.IdempotencyKey,
		"operation":       job.TransferMode,
		"status":          job.Status,
		"phase":           job.Phase, "retry_of": job.RetryOf,
		"block_reason": job.BlockReason,
		"bytes_done":   job.BytesDone,
		"avg_speed":    job.AvgSpeed,
		"error":        job.Error,
		"started_at":   unixOrNil(job.StartedAt.Unix()),
		"ended_at":     unixOrNil(job.EndedAt.Unix()),
	}
	if counts, err := s.st.TransferJobFileCounts(ctx, job.JobID); err == nil {
		payload["files_total"] = counts.Total
		payload["files_done"] = counts.Done
		payload["files_failed"] = counts.Failed
	}
	if job.ResultJSON != "" {
		var manifest map[string]any
		if err := json.Unmarshal([]byte(job.ResultJSON), &manifest); err == nil {
			// Large manifests stay behind /files; the inline copy is a preview.
			if files, err := s.st.ListTransferJobFiles(ctx, job.JobID, "", maxInlineFiles+1); err == nil {
				truncated := len(files) > maxInlineFiles
				if truncated {
					files = files[:maxInlineFiles]
				}
				inline := make([]gin.H, 0, len(files))
				for _, f := range files {
					inline = append(inline, gin.H{"path": f.Path, "size": f.Size, "state": f.State})
				}
				manifest["files"] = inline
				manifest["files_truncated"] = truncated
			}
			payload["result"] = manifest
		}
	}
	return payload
}

func unixOrNil(ts int64) any {
	if ts <= 0 {
		return nil
	}
	return ts
}

func decodeCursor(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func encodeCursor(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func clampInt(raw string, fallback, min, max int) int {
	value := fallback
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		if parsed, err := strconv.Atoi(trimmed); err == nil {
			value = parsed
		}
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

const (
	secretSeparators  = "=: 	\"'"
	secretTerminators = " 	"
)

// redactLogLine strips anything that looks like a credential before a log line
// leaves this service. rclone does not normally log secrets, but a log is the
// one place an operator pastes arbitrary extra arguments into.
func redactLogLine(line string) string {
	lower := strings.ToLower(line)
	for _, marker := range []string{"authorization", "password", "api_key", "apikey", "secret", "token"} {
		idx := strings.Index(lower, marker)
		if idx < 0 {
			continue
		}
		cursor := idx + len(marker)
		// Skip whatever separates the marker from its value: "token=x",
		// "token: x" or "token x" all reach the same value.
		for cursor < len(line) && strings.IndexByte(secretSeparators, line[cursor]) >= 0 {
			cursor++
		}
		end := cursor
		for end < len(line) && strings.IndexByte(secretTerminators, line[end]) < 0 {
			end++
		}
		if end == cursor {
			continue
		}
		return line[:idx+len(marker)] + "=***" + line[end:]
	}
	return line
}

func validCallbackURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("callback url must be http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("callback url must be absolute")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("callback url cannot include credentials or a fragment")
	}
	return value, nil
}
