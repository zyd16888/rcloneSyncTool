package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	url := fmt.Sprintf("http://127.0.0.1:%d/core/stats", port)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("rc status %d: %s", resp.StatusCode, string(b))
	}

	var raw struct {
		Transferring []rcloneTransfer `json:"transferring"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, "", err
	}
	if raw.Transferring == nil {
		return []rcloneTransfer{}, "core/stats.transferring", nil
	}
	return raw.Transferring, "core/stats.transferring", nil
}
