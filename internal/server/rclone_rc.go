package server

import (
	"context"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type rcloneTransfer struct {
	Name  string  `json:"name"`
	Size  int64   `json:"size"`
	Bytes int64   `json:"bytes"`
	Speed float64 `json:"speed"`
	ETA   float64 `json:"eta"`
}

func fetchRcloneTransfers(ctx context.Context, port int) ([]rcloneTransfer, string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	type attempt struct {
		Method string
		URL    string
		Source string
		Body   io.Reader
	}

	attempts := []attempt{
		{
			Method: http.MethodPost,
			URL:    fmt.Sprintf("http://127.0.0.1:%d/core/stats", port),
			Source: "core/stats.transferring",
			Body:   bytes.NewReader([]byte(`{}`)),
		},
		{
			Method: http.MethodGet,
			URL:    fmt.Sprintf("http://127.0.0.1:%d/core/stats", port),
			Source: "core/stats.transferring",
		},
	}

	var lastErr error
	for _, a := range attempts {
		req, _ := http.NewRequestWithContext(ctx, a.Method, a.URL, a.Body)
		if a.Method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("rc status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			continue
		}

		var raw struct {
			Transferring []rcloneTransfer `json:"transferring"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&raw)
		_ = resp.Body.Close()
		if decodeErr != nil {
			lastErr = decodeErr
			continue
		}
		if raw.Transferring == nil {
			return []rcloneTransfer{}, a.Source, nil
		}
		return raw.Transferring, a.Source, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("rc request failed")
	}
	return nil, "", lastErr
}
