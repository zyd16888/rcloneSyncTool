package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"115togd/internal/store"
)

type lsjsonEntry struct {
	Path    string `json:"Path"`
	Size    int64  `json:"Size"`
	ModTime string `json:"ModTime"`
	IsDir   bool   `json:"IsDir"`
}

func scanRule(ctx context.Context, rule store.Rule, settings store.RuntimeSettings) ([]store.ScanEntry, error) {
	var src string
	if rule.SrcKind == "local" {
		src = rule.SrcLocalRoot
	} else {
		src = fmt.Sprintf("%s:%s", rule.SrcRemote, rule.SrcPath)
	}
	args := []string{"lsjson", src, "--recursive", "--files-only"}
	if strings.TrimSpace(settings.RcloneConfigPath) != "" {
		args = append(args, "--config", settings.RcloneConfigPath)
	}
	cmd := exec.CommandContext(ctx, "rclone", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("rclone lsjson: %s", msg)
	}

	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '[' {
		return nil, errors.New("unexpected lsjson output")
	}

	ignoreExts := store.ParseIgnoreExtensions(rule.IgnoreExtensions)
	var out []store.ScanEntry
	for dec.More() {
		var e lsjsonEntry
		if err := dec.Decode(&e); err != nil {
			return nil, err
		}
		if e.IsDir || e.Path == "" {
			continue
		}
		p := strings.TrimLeft(e.Path, "/\\")
		p = strings.ReplaceAll(p, "\\", "/")
		if p == "" {
			continue
		}
		if len(ignoreExts) > 0 {
			lp := strings.ToLower(p)
			ignored := false
			for _, ext := range ignoreExts {
				if strings.HasSuffix(lp, ext) {
					ignored = true
					break
				}
			}
			if ignored {
				continue
			}
		}
		mt, err := time.Parse(time.RFC3339Nano, e.ModTime)
		if err != nil {
			mt, err = time.Parse(time.RFC3339, e.ModTime)
		}
		if err != nil {
			mt = time.Now()
		}
		out = append(out, store.ScanEntry{
			Path:    p,
			Size:    e.Size,
			ModTime: mt,
		})
	}
	_, _ = dec.Token()
	return out, nil
}

type rcStats struct {
	Bytes     int64
	Speed     float64
	Transfers int
	Errors    int
}

func pollRC(ctx context.Context, port int) (rcStats, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	tryPOST := func(url string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		return client.Do(req)
	}

	url1 := fmt.Sprintf("http://127.0.0.1:%d/core/stats", port)
	resp, err := tryPOST(url1)
	if err != nil {
		return rcStats{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Fallback: GET /core/stats (some builds expose GET only).
		req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, url1, nil)
		resp2, err2 := client.Do(req2)
		if err2 != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return rcStats{}, fmt.Errorf("rc status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp2.Body, 4096))
			return rcStats{}, fmt.Errorf("rc status %d: %s", resp2.StatusCode, strings.TrimSpace(string(b)))
		}
		resp = resp2
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return rcStats{}, err
	}
	return rcStats{
		Bytes:     toInt64(m["bytes"]),
		Speed:     toFloat64(m["speed"]),
		Transfers: int(toInt64(m["transfers"])),
		Errors:    int(toInt64(m["errors"])),
	}, nil
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	default:
		return 0
	}
}

func toFloat64(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	default:
		return 0
	}
}

type rcloneRunResult struct {
	BytesDone int64
	AvgSpeed  float64
	Err       error
}

func runRcloneJob(ctx context.Context, rule store.Rule, settings store.RuntimeSettings, port int, filesFromPath, logPath string) rcloneRunResult {
	var src string
	if rule.SrcKind == "local" {
		src = rule.SrcLocalRoot
	} else {
		src = fmt.Sprintf("%s:%s", rule.SrcRemote, rule.SrcPath)
	}
	dst := fmt.Sprintf("%s:%s", rule.DstRemote, rule.DstPath)

	args := []string{
		rule.TransferMode,
		src, dst,
		"--files-from", filesFromPath,
		"--stats", "0",
		"--rc",
		"--rc-no-auth",
		"--rc-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"--log-file", logPath,
		"--log-level", "INFO",
		fmt.Sprintf("--transfers=%d", settings.Transfers),
		fmt.Sprintf("--checkers=%d", settings.Checkers),
	}
	if strings.TrimSpace(settings.RcloneConfigPath) != "" {
		args = append(args, "--config", settings.RcloneConfigPath)
	}
	if settings.BufferSize != "" {
		args = append(args, "--buffer-size", settings.BufferSize)
	}
	if settings.DriveChunkSize != "" {
		args = append(args, "--drive-chunk-size", settings.DriveChunkSize)
	}
	effectiveBwlimit := strings.TrimSpace(rule.Bwlimit)
	if effectiveBwlimit == "" {
		effectiveBwlimit = strings.TrimSpace(settings.Bwlimit)
	}
	if effectiveBwlimit != "" {
		args = append(args, "--bwlimit", effectiveBwlimit)
	}
	if rule.MinFileSizeBytes > 0 {
		args = append(args, "--min-size", fmt.Sprintf("%d", rule.MinFileSizeBytes))
	}
	if strings.TrimSpace(rule.RcloneExtraArgs) != "" {
		parsed, err := ParseRcloneArgs(rule.RcloneExtraArgs)
		if err != nil {
			return rcloneRunResult{Err: err}
		}
		san := SanitizeRcloneArgs(parsed)
		args = append(args, san.Args...)
	}

	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)

	cmd := exec.CommandContext(ctx, "rclone", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return rcloneRunResult{Err: err}
	}

	start := time.Now()
	var last rcStats
	var lastErr error
	readyDeadline := time.NewTimer(10 * time.Second)
	tick := time.NewTicker(settings.MetricsInterval)
	defer readyDeadline.Stop()
	defer tick.Stop()

	ready := false
	for !ready {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return rcloneRunResult{Err: ctx.Err()}
		case <-readyDeadline.C:
			ready = true
		case <-time.After(200 * time.Millisecond):
			s, err := pollRC(ctx, port)
			if err == nil {
				last = s
				ready = true
			}
		}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	for {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = <-done
			return rcloneRunResult{BytesDone: last.Bytes, AvgSpeed: avgSpeed(last.Bytes, start), Err: ctx.Err()}
		case err := <-done:
			if err != nil {
				msg := strings.TrimSpace(stderr.String())
				if msg == "" {
					msg = err.Error()
				}
				return rcloneRunResult{BytesDone: last.Bytes, AvgSpeed: avgSpeed(last.Bytes, start), Err: errors.New(msg)}
			}
			return rcloneRunResult{BytesDone: last.Bytes, AvgSpeed: avgSpeed(last.Bytes, start), Err: lastErr}
		case <-tick.C:
			s, err := pollRC(ctx, port)
			if err != nil {
				lastErr = err
				continue
			}
			last = s
		}
	}
}

func avgSpeed(bytes int64, started time.Time) float64 {
	d := time.Since(started).Seconds()
	if d <= 0 {
		return 0
	}
	return float64(bytes) / d
}
