package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ScanStats struct {
	Discovered, Eligible, Filtered int
	Enqueued                       int64
}
type scanGroup struct {
	Key, Directory, Signature string
	Entries                   []ScanEntry
	Ready                     bool
	ChangedAt                 int64
}

var mediaPartSuffix = regexp.MustCompile(`(?i)([ ._-]+(part|pt|cd|disc|disk)[ ._-]*([0-9]+|[ivx]+|[一二三四五六七八九十]+)|[-_][0-9]{1,2})$`)

func IsMediaFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mkv", ".mp4", ".avi", ".mov", ".wmv", ".m4v", ".ts", ".m2ts", ".mpg", ".mpeg", ".iso", ".vob", ".flv", ".webm":
		return true
	}
	return false
}
func FileGroupKey(rule Rule, name string) (string, string) {
	dir := path.Dir(name)
	if dir == "." {
		dir = ""
	}
	if rule.GroupByDirectory {
		return "dir:" + dir, dir
	}
	if IsMediaFile(name) {
		base := strings.TrimSuffix(path.Base(name), path.Ext(name))
		if stem := mediaPartSuffix.ReplaceAllString(base, ""); stem != base && stem != "" {
			return "parts:" + path.Join(dir, stem), dir
		}
	}
	return "file:" + name, dir
}

func collectScanGroups(rule Rule, entries []ScanEntry, now int64) ([]scanGroup, ScanStats) {
	stats := ScanStats{Discovered: len(entries)}
	groups := map[string]*scanGroup{}
	exts := ParseIgnoreExtensions(rule.IgnoreExtensions)
	markers := map[string]bool{}
	for _, e := range entries {
		if rule.ReadyMarker != "" && path.Base(e.Path) == rule.ReadyMarker {
			markers[path.Dir(e.Path)] = true
			continue
		}
		ignored := false
		for _, ext := range exts {
			if strings.HasSuffix(strings.ToLower(e.Path), ext) {
				ignored = true
				break
			}
		}
		if ignored || e.Size < 0 {
			continue
		}
		key, dir := FileGroupKey(rule, e.Path)
		g := groups[key]
		if g == nil {
			g = &scanGroup{Key: key, Directory: dir, Ready: true, ChangedAt: now}
			groups[key] = g
		}
		g.Entries = append(g.Entries, e)
	}
	var out []scanGroup
	for _, g := range groups {
		eligible := false
		for _, e := range g.Entries {
			if e.Size >= rule.MinFileSizeBytes && (!rule.GroupByDirectory || IsMediaFile(e.Path)) {
				eligible = true
				break
			}
		}
		if !eligible {
			continue
		}
		if !rule.GroupByDirectory && strings.HasPrefix(g.Key, "file:") && !g.Entries[0].ModTime.IsZero() {
			g.ChangedAt = min(now, g.Entries[0].ModTime.UnixMilli())
		}
		if rule.ReadyMarker != "" && rule.GroupByDirectory {
			dir := g.Directory
			if dir == "" {
				dir = "."
			}
			g.Ready = markers[dir]
		}
		sort.Slice(g.Entries, func(i, j int) bool { return g.Entries[i].Path < g.Entries[j].Path })
		h := sha256.New()
		for _, e := range g.Entries {
			fmt.Fprintf(h, "%s\x00%d\x00%s\n", e.Path, e.Size, scanModTime(e.ModTime))
		}
		fmt.Fprintf(h, "ready=%t", g.Ready)
		g.Signature = hex.EncodeToString(h.Sum(nil))
		stats.Eligible += len(g.Entries)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	stats.Filtered = stats.Discovered - stats.Eligible
	return out, stats
}

func TransferEntries(rule Rule, entries []ScanEntry) ([]ScanEntry, error) {
	groups, _ := collectScanGroups(rule, entries, time.Now().UnixMilli())
	var out []ScanEntry
	for _, g := range groups {
		if !g.Ready {
			return nil, fmt.Errorf("影片目录缺少完成标记 %s", rule.ReadyMarker)
		}
		out = append(out, g.Entries...)
	}
	return out, nil
}

func scanModTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (s *Store) ApplyScan(ctx context.Context, rule Rule, entries []ScanEntry) (ScanStats, error) {
	now := time.Now().UnixMilli()
	groups, stats := collectScanGroups(rule, entries, now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return stats, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE files SET source_present=0 WHERE rule_id=?`, rule.ID); err != nil {
		return stats, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE file_groups SET last_seen=-1 WHERE rule_id=?`, rule.ID); err != nil {
		return stats, err
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO files(rule_id,path,size,mod_time,state,last_seen,seen_size,seen_mod_time,job_id,fail_count,last_error,group_key,source_present)
VALUES(?,?,?,?,?,?,0,'',NULL,0,'',?,1)
ON CONFLICT(rule_id,path) DO UPDATE SET
 seen_size=files.size, seen_mod_time=files.mod_time, size=excluded.size, mod_time=excluded.mod_time,
 last_seen=excluded.last_seen, group_key=excluded.group_key, source_present=1,
 state=CASE
  WHEN files.state='transferring' THEN files.state
  WHEN files.state='failed' THEN 'failed'
  WHEN files.state='queued' AND files.job_id IS NOT NULL THEN 'queued'
	WHEN files.state='done' AND ?='move' THEN 'stable'
  WHEN files.state='done' AND excluded.size=files.size AND excluded.mod_time=files.mod_time THEN 'done'
  WHEN excluded.size!=files.size OR excluded.mod_time!=files.mod_time THEN 'new'
  ELSE 'stable' END,
 job_id=CASE WHEN files.state IN ('transferring','failed') OR EXISTS(SELECT 1 FROM jobs WHERE job_id=files.job_id AND status IN ('pending','blocked','running')) THEN files.job_id ELSE NULL END
`)
	if err != nil {
		return stats, err
	}
	defer stmt.Close()
	for _, g := range groups {
		if _, err := tx.ExecContext(ctx, `INSERT INTO file_groups(rule_id,group_key,directory,signature,changed_at,last_seen,ready) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(rule_id,group_key) DO UPDATE SET directory=excluded.directory,
 changed_at=CASE WHEN file_groups.signature=excluded.signature THEN file_groups.changed_at ELSE excluded.last_seen END,
 signature=excluded.signature,last_seen=excluded.last_seen,ready=excluded.ready`, rule.ID, g.Key, g.Directory, g.Signature, g.ChangedAt, now, boolToInt(g.Ready)); err != nil {
			return stats, err
		}
		for _, e := range g.Entries {
			state := "new"
			if !e.ModTime.IsZero() && time.Since(e.ModTime) >= time.Duration(rule.StableSeconds)*time.Second {
				state = "stable"
			}
			if _, err := stmt.ExecContext(ctx, rule.ID, e.Path, e.Size, scanModTime(e.ModTime), state, now, g.Key, rule.TransferMode); err != nil {
				return stats, err
			}
		}
	}
	// Only a complete successful scan may mark entries absent. An active task
	// owns its frozen manifest and will reconcile a missing source on completion.
	if _, err := tx.ExecContext(ctx, `UPDATE files SET state=CASE WHEN state='transferring' OR EXISTS(SELECT 1 FROM jobs WHERE job_id=files.job_id AND status IN ('pending','blocked','running')) THEN state WHEN state='done' THEN 'done' ELSE 'missing' END WHERE rule_id=? AND source_present=0`, rule.ID); err != nil {
		return stats, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM file_groups WHERE rule_id=? AND last_seen=-1 AND NOT EXISTS(SELECT 1 FROM files WHERE rule_id=file_groups.rule_id AND group_key=file_groups.group_key AND state='transferring')`, rule.ID); err != nil {
		return stats, err
	}
	if err := tx.Commit(); err != nil {
		return stats, err
	}
	stats.Enqueued, err = s.EnqueueStable(ctx, rule.ID, rule.BatchSize, 0)
	return stats, err
}

func (s *Store) RepairFileBindings(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET
 state=CASE WHEN fail_count>0 AND EXISTS(SELECT 1 FROM jobs WHERE job_id=files.job_id AND status='failed') THEN 'failed' ELSE 'new' END,
 job_id=NULL
WHERE state IN ('new','stable','queued') AND job_id IS NOT NULL AND job_id<>''
AND NOT EXISTS(SELECT 1 FROM jobs WHERE job_id=files.job_id AND status IN ('pending','blocked','running'))`)
	return err
}
