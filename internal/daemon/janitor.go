package daemon

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"115togd/internal/store"
)

func StartLogJanitor(ctx context.Context, st *store.Store) {
	run := func() {
		rs, err := st.RuntimeSettings(ctx)
		if err != nil {
			log.Printf("janitor: load settings: %v", err)
			return
		}
		days := rs.LogRetentionDays
		if days <= 0 {
			return
		}
		cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		cleanOldJobLogs(rs.LogDir, cutoff)
	}

	run()
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

func cleanOldJobLogs(logDir string, cutoff time.Time) {
	if strings.TrimSpace(logDir) == "" {
		return
	}
	baseDir := filepath.Dir(logDir)
	_ = filepath.WalkDir(logDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".log") {
			return nil
		}
		fi, err := os.Stat(p)
		if err != nil {
			return nil
		}
		if !fi.ModTime().Before(cutoff) {
			return nil
		}

		rel, err := filepath.Rel(logDir, p)
		if err != nil {
			_ = os.Remove(p)
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) >= 2 {
			ruleID := parts[0]
			jobID := strings.TrimSuffix(parts[len(parts)-1], ".log")
			if ruleID != "" && jobID != "" {
				_ = os.Remove(p)
				_ = os.RemoveAll(filepath.Join(baseDir, "jobs", ruleID, jobID))
				_ = os.Remove(filepath.Join(logDir, ruleID))
				_ = os.Remove(filepath.Join(baseDir, "jobs", ruleID))
				return nil
			}
		}
		_ = os.Remove(p)
		return nil
	})
}

