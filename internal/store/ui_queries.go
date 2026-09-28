package store

import (
	"context"
	"strings"
	"time"
)

type JobFileSummary struct {
	Total, Done, Failed int
	Bytes               int64
	FirstPath           string
}

// UI snapshots project request metadata without copying each job's complete
// file manifest or callback credentials into the list response.
func uiJobColumns() string {
	meta := "CASE WHEN json_valid(request_snapshot) THEN json_object(" +
		"'operation',json_extract(request_snapshot,'$.operation')," +
		"'source_subpath',json_extract(request_snapshot,'$.source_subpath')," +
		"'destination_subpath',json_extract(request_snapshot,'$.destination_subpath')," +
		"'rule',json_extract(request_snapshot,'$.rule')," +
		"'prepared',json_extract(request_snapshot,'$.prepared')) ELSE '' END"
	return strings.Replace(transferJobColumns, "COALESCE(request_snapshot,'')", meta, 1)
}

func (s *Store) UIJobs(ctx context.Context, f JobFilter, limit, offset int, sortBy, direction string, freshSince time.Time) ([]TransferJob, error) {
	where, args := buildJobsWhere(f)
	order := "COALESCE(NULLIF(created_at,0),started_at)"
	switch sortBy {
	case "name":
		order = "COALESCE(NULLIF(" + jobSourceSubpathSQL() + ",''),NULLIF(group_key,''),rule_id)"
	case "bytes":
		order = "bytes_done"
	case "status":
		order = "status"
	case "speed":
		order = "CASE WHEN status='running' AND phase IN ('copying','') THEN COALESCE((SELECT speed FROM job_metrics m WHERE m.job_id=jobs.job_id AND m.ts>=? ORDER BY m.ts DESC LIMIT 1),0) ELSE 0 END"
		args = append(args, freshSince.UnixMilli())
	}
	dir := "DESC"
	if direction == "asc" {
		dir = "ASC"
	}
	args = append(args, max(1, min(100, limit)), max(0, offset))
	rows, err := s.db.QueryContext(ctx, "SELECT "+uiJobColumns()+" FROM jobs "+where+" ORDER BY "+order+" "+dir+",job_id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TransferJob
	for rows.Next() {
		job, err := scanTransferJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func (s *Store) UIJob(ctx context.Context, id string) (TransferJob, bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+uiJobColumns()+" FROM jobs WHERE job_id=?", id)
	if err != nil {
		return TransferJob{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return TransferJob{}, false, rows.Err()
	}
	job, err := scanTransferJob(rows.Scan)
	return job, err == nil, err
}

func idsClause(ids []string) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

func (s *Store) JobFileSummaries(ctx context.Context, ids []string) (map[string]JobFileSummary, error) {
	out := map[string]JobFileSummary{}
	if len(ids) == 0 {
		return out, nil
	}
	clause, args := idsClause(ids)
	rows, err := s.db.QueryContext(ctx, "SELECT job_id,COUNT(*),COALESCE(SUM(size),0),SUM(state='done'),SUM(state='failed'),MIN(path) FROM transfer_job_files WHERE job_id IN ("+clause+") GROUP BY job_id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var f JobFileSummary
		if err := rows.Scan(&id, &f.Total, &f.Bytes, &f.Done, &f.Failed, &f.FirstPath); err != nil {
			return nil, err
		}
		out[id] = f
	}
	return out, rows.Err()
}

func (s *Store) JobMetricsFor(ctx context.Context, ids []string) (map[string]JobMetric, error) {
	out := map[string]JobMetric{}
	if len(ids) == 0 {
		return out, nil
	}
	clause, args := idsClause(ids)
	rows, err := s.db.QueryContext(ctx, "SELECT job_id,ts,bytes,speed,transfers,errors FROM job_metrics m WHERE job_id IN ("+clause+") AND ts=(SELECT MAX(ts) FROM job_metrics newest WHERE newest.job_id=m.job_id)", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m JobMetric
		var ts int64
		if err := rows.Scan(&m.JobID, &ts, &m.Bytes, &m.Speed, &m.Transfers, &m.Errors); err != nil {
			return nil, err
		}
		m.Ts = time.UnixMilli(ts)
		out[m.JobID] = m
	}
	return out, rows.Err()
}

func (s *Store) ActiveRetriesFor(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	clause, args := idsClause(ids)
	rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT retry_of FROM jobs WHERE retry_of IN ("+clause+") AND status IN ('pending','blocked','running')", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) JobStatusCounts(ctx context.Context, filter JobFilter) (map[string]int, error) {
	filter.Status = ""
	where, args := buildJobsWhere(filter)
	rows, err := s.db.QueryContext(ctx, "SELECT status,COUNT(*) FROM jobs "+where+" GROUP BY status", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
		out["all"] += n
	}
	return out, rows.Err()
}

type RuleTransferActivity struct {
	Speed           float64
	Phase           string
	Blocked         int
	Failed          int
	IgnoredFailures int
	LatestError     string
}

func (s *Store) RuleTransferActivities(ctx context.Context, freshSince time.Time) (map[string]RuleTransferActivity, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT rule_id,status,phase,error,block_reason,error_ignored,"+
		"CASE WHEN status='running' AND phase IN ('copying','') THEN COALESCE((SELECT speed FROM job_metrics m WHERE m.job_id=j.job_id AND ts>=? ORDER BY ts DESC LIMIT 1),0) ELSE 0 END "+
		"FROM jobs j WHERE status IN ('running','pending','blocked') OR ("+currentRuleTaskFailureSQL()+") ORDER BY created_at,job_id", freshSince.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RuleTransferActivity{}
	for rows.Next() {
		var id, status, phase, message, reason string
		var speed float64
		var ignored bool
		if err := rows.Scan(&id, &status, &phase, &message, &reason, &ignored, &speed); err != nil {
			return nil, err
		}
		a := out[id]
		a.Speed += speed
		if status == "running" && (a.Phase == "" || phase == "copying") {
			a.Phase = phase
		}
		if status == "blocked" {
			a.Blocked++
			if a.LatestError == "" {
				a.LatestError = message
			}
		}
		if status == "failed" {
			if ignored {
				a.IgnoredFailures++
			} else {
				a.Failed++
				a.LatestError = message
			}
		}
		out[id] = a
	}
	return out, rows.Err()
}

func (s *Store) RuleUsageTotals(ctx context.Context, since time.Time) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT rule_id,SUM(bytes) FROM ("+
		"SELECT rule_id,bytes FROM transfer_usage WHERE ts>=? UNION ALL "+
		"SELECT rule_id,bytes_done AS bytes FROM jobs j WHERE ended_at>=? AND status NOT IN ('pending','blocked','running') AND NOT EXISTS(SELECT 1 FROM transfer_usage u WHERE u.job_id=j.job_id)) GROUP BY rule_id", since.UnixMilli(), since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *Store) FreshRealtimeSummary(ctx context.Context, ruleID string, freshSince time.Time) (RealtimeSummary, error) {
	where := " WHERE status='running'"
	args := []any{freshSince.UnixMilli()}
	if ruleID != "" {
		where += " AND rule_id=?"
		args = append(args, ruleID)
	}
	var result RealtimeSummary
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(CASE WHEN phase IN ('copying','') THEN COALESCE((SELECT speed FROM job_metrics m WHERE m.job_id=j.job_id AND ts>=? ORDER BY ts DESC LIMIT 1),0) ELSE 0 END),0) FROM jobs j"+where, args...).Scan(&result.RunningJobs, &result.SpeedTotal)
	return result, err
}
