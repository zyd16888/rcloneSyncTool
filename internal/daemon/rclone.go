package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"os/exec"

	"strconv"
	"strings"
	"time"

	"115togd/internal/store"
)

type lsjsonEntry struct {
	ID      string `json:"ID"`
	Path    string `json:"Path"`
	Size    int64  `json:"Size"`
	ModTime string `json:"ModTime"`
	IsDir   bool   `json:"IsDir"`
}

func scanRule(ctx context.Context, rule store.Rule, settings store.RuntimeSettings) ([]store.ScanEntry, error) {
	if err := ValidateRcloneArgs(rule.RcloneExtraArgs); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.ScanTimeout)
	defer cancel()
	args := []string{"lsjson", sourceSpec(rule), "--recursive", "--files-only"}
	extra, err := ParseRcloneArgs(rule.RcloneExtraArgs)
	if err != nil {
		return nil, err
	}
	args = append(args, extra...)
	if settings.RcloneConfigPath != "" {
		args = append(args, "--config", settings.RcloneConfigPath)
	}
	cmd := exec.CommandContext(ctx, "rclone", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	finished := false
	defer func() {
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	dec := json.NewDecoder(stdout)
	token, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("rclone lsjson: %w", err)
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '[' {
		return nil, errors.New("unexpected lsjson output")
	}
	var out []store.ScanEntry
	for dec.More() {
		var e lsjsonEntry
		if err := dec.Decode(&e); err != nil {
			return nil, err
		}
		if e.IsDir || e.Path == "" {
			continue
		}
		p := strings.ReplaceAll(e.Path, "\\", "/")
		if !validRelativeFile(p) {
			return nil, fmt.Errorf("源文件路径无法安全表示：%q", p)
		}
		mt, _ := time.Parse(time.RFC3339Nano, e.ModTime)
		out = append(out, store.ScanEntry{Path: p, Size: e.Size, ModTime: mt})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	err = cmd.Wait()
	finished = true
	if err != nil {
		return nil, fmt.Errorf("rclone lsjson: %s", redactMessage(strings.TrimSpace(stderr.String())))
	}
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

func avgSpeed(bytes int64, started time.Time) float64 {
	d := time.Since(started).Seconds()
	if d <= 0 {
		return 0
	}
	return float64(bytes) / d
}
