package daemon

import (
	"115togd/internal/store"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func StartLogJanitor(ctx context.Context, st *store.Store) {
	run := func() {
		settings, err := st.RuntimeSettings(ctx)
		if err != nil {
			log.Printf("janitor settings: %v", err)
			return
		}
		if settings.LogRetentionDays > 0 {
			if err := cleanCompletedJobLogs(ctx, st, settings.LogDir, time.Now().Add(-time.Duration(settings.LogRetentionDays)*24*time.Hour)); err != nil {
				log.Printf("janitor logs: %v", err)
			}
		}
		cutoff := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
		_, _ = st.DB().ExecContext(ctx, `DELETE FROM job_metrics WHERE ts<? AND EXISTS(SELECT 1 FROM jobs WHERE job_id=job_metrics.job_id AND status IN ('done','failed','terminated'))`, cutoff)
		_, _ = st.DB().ExecContext(ctx, `DELETE FROM transfer_usage WHERE ts<? AND EXISTS(SELECT 1 FROM jobs WHERE job_id=transfer_usage.job_id AND status IN ('done','failed','terminated') AND ended_at<?)`, cutoff, time.Now().Add(-30*24*time.Hour).Unix())
	}
	run()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
func cleanCompletedJobLogs(ctx context.Context, st *store.Store, logDir string, cutoff time.Time) error {
	rows, err := st.DB().QueryContext(ctx, `SELECT job_id,rule_id,log_path FROM jobs WHERE status IN ('done','failed','terminated') AND ended_at>0 AND ended_at<?`, cutoff.Unix())
	if err != nil {
		return err
	}
	type artifact struct{ id, rule, path string }
	var artifacts []artifact
	for rows.Next() {
		var a artifact
		if err := rows.Scan(&a.id, &a.rule, &a.path); err != nil {
			rows.Close()
			return err
		}
		artifacts = append(artifacts, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, a := range artifacts {
		if !safeArtifactComponent(a.id) || !safeArtifactComponent(a.rule) {
			continue
		}
		expected := filepath.Join(logDir, a.rule, a.id+".log")
		absoluteExpected, _ := filepath.Abs(expected)
		absoluteRecorded, _ := filepath.Abs(a.path)
		if absoluteExpected != absoluteRecorded {
			continue
		}
		if err := os.Remove(expected); err != nil && !os.IsNotExist(err) {
			return err
		}
		dir := filepath.Join(filepath.Dir(logDir), "jobs", a.rule, a.id)
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}
func safeArtifactComponent(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\:\x00")
}
