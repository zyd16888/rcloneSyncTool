package server

import (
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"path"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"115togd/internal/daemon"
	"115togd/internal/store"
)

//go:embed templates/*.html static
var content embed.FS

type Server struct {
	st         *store.Store
	supervisor *daemon.Supervisor
	logDir     string
	appLogPath string

	pages map[string]*template.Template
}

func New(st *store.Store, supervisor *daemon.Supervisor, logDir string, appLogPath string) http.Handler {
	s := &Server{
		st:         st,
		supervisor: supervisor,
		logDir:     logDir,
		appLogPath: appLogPath,
	}
	funcs := template.FuncMap{
		"since": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			d := time.Since(t).Round(time.Second)
			return d.String()
		},
		"ts": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Format("2006-01-02 15:04:05")
		},
		"hasPrefix": strings.HasPrefix,
		"humanBytes": humanBytes,
	}
	s.pages = map[string]*template.Template{}
	files, err := fs.Glob(content, "templates/*.html")
	if err != nil {
		panic(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "/layout.html") || strings.HasSuffix(f, "layout.html") {
			continue
		}
		name := strings.TrimSuffix(path.Base(f), ".html")
		t := template.New("layout").Funcs(funcs)
		t = template.Must(t.ParseFS(content, "templates/layout.html", f))
		s.pages[name] = t
	}

	staticFS, _ := fs.Sub(content, "static")
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Cache-Control", "no-store")
		c.Next()
	})

	r.GET("/login", s.loginGet)
	r.POST("/login", s.loginPost)
	r.POST("/logout", s.logoutPost)

	r.Use(s.authMiddleware())

	r.GET("/", s.dashboard)

	r.GET("/remotes", s.remotesList)

	r.GET("/rclone/config", s.rcloneConfigGet)
	r.POST("/rclone/config/save", s.rcloneConfigSavePost)

	r.GET("/rules", s.rulesList)
	r.GET("/rules/edit", s.ruleEditGet)
	r.POST("/rules/save", s.ruleSavePost)
	r.POST("/rules/delete", s.ruleDeletePost)
	r.POST("/rules/toggle", s.ruleTogglePost)
	r.POST("/rules/scan", s.ruleScanPost)
	r.POST("/rules/retry_failed", s.ruleRetryFailedPost)

	r.GET("/manual", s.manualGet)
	r.POST("/manual/start", s.manualStartPost)

	r.GET("/jobs", s.jobsList)
	r.GET("/jobs/view", s.jobView)
	r.POST("/jobs/terminate", s.jobTerminatePost)
	r.GET("/api/job", s.apiJob)
	r.GET("/api/job/log/stream", s.apiJobLogStream)
	r.GET("/api/job/transfers", s.apiJobTransfers)

	r.GET("/api/fs/list", s.apiFSList)
	r.GET("/api/rclone/dirs", s.apiRcloneDirs)

	r.GET("/api/stats/now", s.apiStatsNow)

	r.GET("/logs", s.logsPage)
	r.GET("/api/log/daemon/stream", s.apiDaemonLogStream)

	r.GET("/settings", s.settingsGet)
	r.POST("/settings/save", s.settingsSavePost)
	r.GET("/api/rclone/check", s.apiRcloneCheck)

	r.StaticFS("/static", http.FS(staticFS))

	return r
}

func (s *Server) render(c *gin.Context, name string, data any) {
	c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if m, ok := data.(map[string]any); ok {
		s.injectBase(c, m)
	}
	t, ok := s.pages[name]
	if !ok {
		c.String(http.StatusInternalServerError, "template not found")
		return
	}
	if err := t.ExecuteTemplate(c.Writer, "layout", data); err != nil {
		log.Printf("render %s: %v", name, err)
		c.String(http.StatusInternalServerError, "template error")
	}
}

func (s *Server) redirect(c *gin.Context, p string) {
	c.Redirect(http.StatusSeeOther, p)
}

func (s *Server) dashboard(c *gin.Context) {
	ctx := c.Request.Context()
	rules, _ := s.st.ListRules(ctx)
	type ruleRow struct {
		Rule   store.Rule
		Counts store.FileStateCounts
	}
	var rows []ruleRow
	for _, rule := range rules {
		counts, _ := s.st.RuleFileCounts(ctx, rule.ID)
		rows = append(rows, ruleRow{Rule: rule, Counts: counts})
	}
	jobsPage := atoiDefault(c.Query("jobs_page"), 1)
	jobsPageSize := normalizePageSize(c.Query("jobs_page_size"), 20)
	if jobsPage <= 0 {
		jobsPage = 1
	}
	totalJobs, _ := s.st.CountJobs(ctx)
	totalPages := (totalJobs + jobsPageSize - 1) / jobsPageSize
	if totalPages <= 0 {
		totalPages = 1
	}
	if jobsPage > totalPages {
		jobsPage = totalPages
	}
	offset := (jobsPage - 1) * jobsPageSize
	jobs, _ := s.st.ListJobsPage(ctx, jobsPageSize, offset)
	type jobRow struct {
		Job    store.Job
		Metric store.JobMetric
		HasM   bool
	}
	var jobRows []jobRow
	for _, j := range jobs {
		m, ok, _ := s.st.LatestJobMetric(ctx, j.JobID)
		jobRows = append(jobRows, jobRow{Job: j, Metric: m, HasM: ok})
	}
	totalBytes, _ := s.st.TotalBytesDone(ctx)
	totalSpeed, _ := s.st.TotalSpeedRunning(ctx)
	runningJobs, _ := s.st.CountRunningJobsAll(ctx)
	settings, _ := s.st.RuntimeSettings(ctx)
	hasPrev := jobsPage > 1
	hasNext := jobsPage < totalPages
	s.render(c, "dashboard", map[string]any{
		"Active":   "dashboard",
		"Rules":    rows,
		"Jobs":     jobRows,
		"LogDir":   s.logDir,
		"TotalBytes": totalBytes,
		"TotalSpeed": totalSpeed,
		"RunningJobs": runningJobs,
		"RcloneConfig": settings.RcloneConfigPath,
		"JobsPage":      jobsPage,
		"JobsPageSize":  jobsPageSize,
		"JobsTotal":     totalJobs,
		"JobsTotalPages": totalPages,
		"JobsHasPrev":   hasPrev,
		"JobsHasNext":   hasNext,
		"JobsPrevURL":   fmt.Sprintf("/?jobs_page=%d&jobs_page_size=%d", maxInt(1, jobsPage-1), jobsPageSize),
		"JobsNextURL":   fmt.Sprintf("/?jobs_page=%d&jobs_page_size=%d", minInt(totalPages, jobsPage+1), jobsPageSize),
	})
}

func (s *Server) remotesList(c *gin.Context) {
	ctx := c.Request.Context()
	remotes, err := s.listRcloneRemotes(ctx)
	s.render(c, "remotes", map[string]any{
		"Active":  "remotes",
		"Remotes": remotes,
		"Error":   errString(err),
	})
}

func (s *Server) rulesList(c *gin.Context) {
	ctx := c.Request.Context()
	rules, _ := s.st.ListRules(ctx)
	type ruleRow struct {
		Rule   store.Rule
		Counts store.FileStateCounts
	}
	var rows []ruleRow
	for _, rule := range rules {
		counts, _ := s.st.RuleFileCounts(ctx, rule.ID)
		rows = append(rows, ruleRow{Rule: rule, Counts: counts})
	}
	s.render(c, "rules", map[string]any{
		"Active": "rules",
		"Rules": rows,
	})
}

func (s *Server) ruleEditGet(c *gin.Context) {
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.Query("id"))
	var rule store.Rule
	if id != "" {
		if got, ok, _ := s.st.GetRule(ctx, id); ok {
			rule = got
		}
	}
	if rule.ID == "" {
		rule.Enabled = true
		rule.SrcKind = "remote"
		rule.LocalWatch = true
		rule.TransferMode = "copy"
		rule.MaxParallelJobs = 1
		rule.ScanIntervalSec = 15
		rule.StableSeconds = 60
		rule.BatchSize = 100
	}
	remotes, err := s.listRcloneRemotes(ctx)
	s.render(c, "rule_edit", map[string]any{
		"Active":  "rules",
		"Rule":    rule,
		"Remotes": remotes,
		"Error":   errString(err),
	})
}

func (s *Server) manualGet(c *gin.Context) {
	ctx := c.Request.Context()
	remotes, err := s.listRcloneRemotes(ctx)
	s.render(c, "manual", map[string]any{
		"Active":  "rules",
		"Remotes": remotes,
		"Error":   errString(err),
	})
}

func (s *Server) manualStartPost(c *gin.Context) {
	ctx := c.Request.Context()

	minSize, err := parseSizeBytes(c.PostForm("min_file_size"))
	if err != nil {
		c.String(http.StatusBadRequest, "最小文件大小格式错误：%v（示例：10M / 1.5G / 0 / 留空）", err)
		return
	}

	if strings.TrimSpace(c.PostForm("rclone_extra_args")) != "" {
		if _, err := daemon.ParseRcloneArgs(c.PostForm("rclone_extra_args")); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
	}

	jobID := newID()
	ruleID := "manual_" + jobID
	rule := store.Rule{
		ID:               ruleID,
		SrcKind:          c.PostForm("src_kind"),
		SrcRemote:        c.PostForm("src_remote"),
		SrcPath:          c.PostForm("src_path"),
		SrcLocalRoot:     c.PostForm("src_local_root"),
		DstRemote:        c.PostForm("dst_remote"),
		DstPath:          c.PostForm("dst_path"),
		TransferMode:     c.PostForm("transfer_mode"),
		RcloneExtraArgs:  c.PostForm("rclone_extra_args"),
		Bwlimit:          c.PostForm("bwlimit"),
		MinFileSizeBytes: minSize,
		IsManual:         true,
		Enabled:          false,
		MaxParallelJobs:  1,
		ScanIntervalSec:  15,
		StableSeconds:    60,
		BatchSize:        100,
	}
	if err := s.st.UpsertRule(ctx, rule); err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}

	settings, err := s.st.RuntimeSettings(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "load settings: %v", err)
		return
	}
	logPath := filepath.Join(settings.LogDir, rule.ID, jobID+".log")

	j := store.Job{
		JobID:        jobID,
		RuleID:       rule.ID,
		TransferMode: rule.TransferMode,
		StartedAt:    time.Now(),
		LogPath:      logPath,
	}
	if err := s.st.CreateJobRowPending(ctx, j); err != nil {
		c.String(http.StatusInternalServerError, "create job: %v", err)
		return
	}

	baseDir := filepath.Dir(settings.LogDir)
	jobDir := filepath.Join(baseDir, "jobs", rule.ID, jobID)
	_ = os.MkdirAll(jobDir, 0o755)
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)

	s.supervisor.StartManualJob(rule, jobID, logPath)
	s.redirect(c, "/jobs/view?id="+jobID)
}

func (s *Server) ruleSavePost(c *gin.Context) {
	ctx := c.Request.Context()
	minSize, err := parseSizeBytes(c.PostForm("min_file_size"))
	if err != nil {
		c.String(http.StatusBadRequest, "最小文件大小格式错误：%v（示例：10M / 1.5G / 0 / 留空）", err)
		return
	}
	if strings.TrimSpace(c.PostForm("rclone_extra_args")) != "" {
		if _, err := daemon.ParseRcloneArgs(c.PostForm("rclone_extra_args")); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
	}
	rule := store.Rule{
		ID:              c.PostForm("id"),
		SrcKind:         c.PostForm("src_kind"),
		SrcRemote:       c.PostForm("src_remote"),
		SrcPath:         c.PostForm("src_path"),
		SrcLocalRoot:    c.PostForm("src_local_root"),
		LocalWatch:      store.ParseEnabled(c.PostForm("local_watch_enabled")),
		DstRemote:       c.PostForm("dst_remote"),
		DstPath:         c.PostForm("dst_path"),
		TransferMode:    c.PostForm("transfer_mode"),
		RcloneExtraArgs: c.PostForm("rclone_extra_args"),
		Bwlimit:         c.PostForm("bwlimit"),
		MinFileSizeBytes: minSize,
		MaxParallelJobs: atoiDefault(c.PostForm("max_parallel_jobs"), 1),
		ScanIntervalSec: atoiDefault(c.PostForm("scan_interval_sec"), 15),
		StableSeconds:   atoiDefault(c.PostForm("stable_seconds"), 60),
		BatchSize:       atoiDefault(c.PostForm("batch_size"), 100),
		Enabled:         store.ParseEnabled(c.PostForm("enabled")),
	}
	if err := s.st.UpsertRule(ctx, rule); err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}
	if !rule.Enabled && s.supervisor != nil {
		s.supervisor.StopRule(rule.ID)
	}
	s.redirect(c, "/rules")
}

func (s *Server) ruleDeletePost(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.PostForm("id")
	_ = s.st.DeleteRule(ctx, id)
	s.redirect(c, "/rules")
}

func (s *Server) ruleTogglePost(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.PostForm("id")
	enabled := store.ParseEnabled(c.PostForm("enabled"))
	rule, ok, err := s.st.GetRule(ctx, id)
	if err != nil || !ok {
		c.String(http.StatusNotFound, "rule not found")
		return
	}
	rule.Enabled = enabled
	if err := s.st.UpsertRule(ctx, rule); err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}
	if !enabled && s.supervisor != nil {
		s.supervisor.StopRule(id)
	}
	s.redirect(c, "/rules")
}

func (s *Server) ruleScanPost(c *gin.Context) {
	id := c.PostForm("id")
	_ = s.supervisor.TriggerScan(id)
	s.redirect(c, "/rules")
}

func (s *Server) ruleRetryFailedPost(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.PostForm("id")
	_, _ = s.st.RetryFailed(ctx, id, 10000)
	s.redirect(c, "/rules")
}

func (s *Server) jobsList(c *gin.Context) {
	ctx := c.Request.Context()
	page := atoiDefault(c.Query("page"), 1)
	pageSize := normalizePageSize(c.Query("page_size"), 50)
	if page <= 0 {
		page = 1
	}
	filter := store.JobFilter{
		RuleID:       strings.TrimSpace(c.Query("rule_id")),
		Status:       normalizeJobStatus(c.Query("status")),
		TransferMode: normalizeTransferMode(c.Query("mode")),
		Query:        strings.TrimSpace(c.Query("q")),
	}
	total, _ := s.st.CountJobsFiltered(ctx, filter)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages <= 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize
	jobs, _ := s.st.ListJobsPageFiltered(ctx, pageSize, offset, filter)
	type row struct {
		Job    store.Job
		Metric store.JobMetric
		HasM   bool
	}
	var rows []row
	for _, j := range jobs {
		m, ok, _ := s.st.LatestJobMetric(ctx, j.JobID)
		rows = append(rows, row{Job: j, Metric: m, HasM: ok})
	}
	hasPrev := page > 1
	hasNext := page < totalPages
	rules, _ := s.st.ListRules(ctx)
	prevURL := s.jobsListURL(page-1, pageSize, filter)
	nextURL := s.jobsListURL(page+1, pageSize, filter)
	s.render(c, "jobs", map[string]any{
		"Active": "jobs",
		"Jobs": rows,
		"Rules": rules,
		"F": filter,
		"SelfURL": c.Request.URL.RequestURI(),
		"Page": page,
		"PageSize": pageSize,
		"Total": total,
		"TotalPages": totalPages,
		"HasPrev": hasPrev,
		"HasNext": hasNext,
		"PrevURL": prevURL,
		"NextURL": nextURL,
	})
}

func normalizePageSize(s string, def int) int {
	size := atoiDefault(s, def)
	switch size {
	case 10, 20, 50, 100:
		return size
	default:
		return def
	}
}

func normalizeJobStatus(s string) string {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "running", "done", "failed", "terminated":
		return strings.TrimSpace(strings.ToLower(s))
	default:
		return ""
	}
}

func normalizeTransferMode(s string) string {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "copy", "move":
		return strings.TrimSpace(strings.ToLower(s))
	default:
		return ""
	}
}

func (s *Server) jobsListURL(page, pageSize int, f store.JobFilter) string {
	if page <= 0 {
		page = 1
	}
	v := url.Values{}
	v.Set("page", fmt.Sprintf("%d", page))
	v.Set("page_size", fmt.Sprintf("%d", pageSize))
	if strings.TrimSpace(f.RuleID) != "" {
		v.Set("rule_id", strings.TrimSpace(f.RuleID))
	}
	if strings.TrimSpace(f.Status) != "" {
		v.Set("status", strings.TrimSpace(f.Status))
	}
	if strings.TrimSpace(f.TransferMode) != "" {
		v.Set("mode", strings.TrimSpace(f.TransferMode))
	}
	if strings.TrimSpace(f.Query) != "" {
		v.Set("q", strings.TrimSpace(f.Query))
	}
	return "/jobs?" + v.Encode()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Server) jobView(c *gin.Context) {
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.Query("id"))
	job, ok, _ := s.st.GetJob(ctx, id)
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	rule, _, _ := s.st.GetRule(ctx, job.RuleID)
	s.render(c, "job_view", map[string]any{
		"Active": "jobs",
		"Job":  job,
		"Rule": rule,
	})
}

func (s *Server) apiJob(c *gin.Context) {
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.Query("id"))
	job, ok, err := s.st.GetJob(ctx, id)
	if err != nil || !ok {
		c.Status(http.StatusNotFound)
		return
	}
	metric, hasM, _ := s.st.LatestJobMetric(ctx, job.JobID)
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(c.Writer).Encode(map[string]any{
		"job":     job,
		"metric":  metric,
		"hasMetric": hasM,
	})
}

func (s *Server) apiStatsNow(c *gin.Context) {
	ctx := c.Request.Context()
	ruleID := strings.TrimSpace(c.Query("rule_id"))
	sum, err := s.st.RealtimeSummary(ctx, ruleID)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(c.Writer).Encode(map[string]any{
		"ts": time.Now().UnixMilli(),
		"ruleID": ruleID,
		"bytesTotal": sum.BytesTotal,
		"speedTotal": sum.SpeedTotal,
		"runningJobs": sum.RunningJobs,
	})
}

func (s *Server) apiJobTransfers(c *gin.Context) {
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.Query("id"))
	job, ok, err := s.st.GetJob(ctx, id)
	if err != nil || !ok {
		c.Status(http.StatusNotFound)
		return
	}
	if job.Status != "running" || job.RcPort <= 0 {
		c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(c.Writer).Encode(map[string]any{
			"jobID": job.JobID,
			"running": false,
			"transfers": []any{},
		})
		return
	}
	transfers, source, err := fetchRcloneTransfers(ctx, job.RcPort)
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err != nil {
		_ = json.NewEncoder(c.Writer).Encode(map[string]any{
			"jobID": job.JobID,
			"running": true,
			"error": err.Error(),
			"transfers": []any{},
		})
		return
	}
	_ = json.NewEncoder(c.Writer).Encode(map[string]any{
		"jobID": job.JobID,
		"running": true,
		"source": source,
		"transfers": transfers,
	})
}

func (s *Server) settingsGet(c *gin.Context) {
	ctx := c.Request.Context()
	all, _ := s.st.ListSettings(ctx)
	m := map[string]string{}
	for _, kv := range all {
		m[kv.Key] = kv.Value
	}
	s.render(c, "settings", map[string]any{
		"Active":   "settings",
		"S":        m,
		"LogDir":   s.logDir,
	})
}

func (s *Server) settingsSavePost(c *gin.Context) {
	ctx := c.Request.Context()
	passwordChanged := false
	if p := strings.TrimSpace(c.PostForm("ui_password")); p != "" {
		if p != strings.TrimSpace(c.PostForm("ui_password2")) {
			c.String(http.StatusBadRequest, "两次输入的密码不一致")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
		if err != nil {
			c.String(http.StatusInternalServerError, "密码加密失败：%v", err)
			return
		}
		if err := s.st.SetSetting(ctx, authPasswordHashKey, string(hash)); err != nil {
			c.String(http.StatusInternalServerError, "保存密码失败：%v", err)
			return
		}
		passwordChanged = true
	}

	for _, key := range []string{
		"rclone_config_path",
		"log_retention_days",
		"global_max_jobs",
		"rc_port_start",
		"rc_port_end",
		"rclone_transfers",
		"rclone_checkers",
		"rclone_buffer_size",
		"rclone_drive_chunk_size",
		"rclone_bwlimit",
		"metrics_interval_ms",
		"scheduler_tick_ms",
	} {
		v := strings.TrimSpace(c.PostForm(key))
		if key == "rclone_config_path" {
			_ = s.st.SetSetting(ctx, key, v)
			continue
		}
		if v == "" {
			continue
		}
		_ = s.st.SetSetting(ctx, key, v)
	}
	if passwordChanged {
		clearAuthCookie(c)
		s.redirect(c, "/login?next=%2Fsettings")
		return
	}
	s.redirect(c, "/settings")
}

func (s *Server) jobTerminatePost(c *gin.Context) {
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.PostForm("id"))
	if id == "" {
		c.String(http.StatusBadRequest, "missing job id")
		return
	}
	job, ok, _ := s.st.GetJob(ctx, id)
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	if job.Status != "running" {
		c.String(http.StatusConflict, "job is not running")
		return
	}
	if !s.supervisor.TerminateJob(id) {
		c.String(http.StatusConflict, "terminate failed: job not found in registry")
		return
	}
	next := strings.TrimSpace(c.PostForm("next"))
	if next == "" || !strings.HasPrefix(next, "/") {
		next = "/jobs"
	}
	s.redirect(c, next)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func atoiDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func parseKV(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			continue
		}
		out[k] = v
	}
	return out
}

func serializeKV(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(v)
		b.WriteString("\n")
	}
	return b.String()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	suffix := string("KMGTPE"[exp]) + "iB"
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + suffix
}
