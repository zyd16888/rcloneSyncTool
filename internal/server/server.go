package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

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

	doneMu    sync.Mutex
	doneCache map[string]*doneCountCacheEntry
}

func New(st *store.Store, supervisor *daemon.Supervisor, logDir string, appLogPath string) http.Handler {
	s := &Server{
		st:         st,
		supervisor: supervisor,
		logDir:     logDir,
		appLogPath: appLogPath,
		doneCache:  map[string]*doneCountCacheEntry{},
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
		"hasPrefix":  strings.HasPrefix,
		"humanBytes": humanBytes,
		"humanSpeed": humanSpeed,
	}
	for name, fn := range uiTemplateFuncs() {
		funcs[name] = fn
	}
	s.pages = map[string]*template.Template{}
	files, err := fs.Glob(content, "templates/*.html")
	if err != nil {
		panic(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "/layout.html") || strings.HasPrefix(path.Base(f), "_") {
			continue
		}
		name := strings.TrimSuffix(path.Base(f), ".html")
		t := template.New("layout").Funcs(funcs)
		t = template.Must(t.ParseFS(content, "templates/layout.html", "templates/_*.html", f))
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

	// Machine API. Registered before the cookie middleware so it carries only
	// bearer authentication; gin copies the parent handler chain when the group
	// is created, so a later r.Use cannot leak session auth into these routes.
	v1 := r.Group("/api/v1", s.apiTokenMiddleware())
	v1.GET("/version", s.apiV1Version)
	v1.GET("/capabilities", s.apiV1Capabilities)
	v1.GET("/health", s.apiV1Health)
	v1.POST("/transfer-jobs", s.createTransferJob)
	v1.GET("/transfer-jobs/:id", s.getTransferJob)
	v1.GET("/transfer-jobs/by-external-id/:external_id", s.getTransferJobByExternalID)
	v1.GET("/transfer-jobs/:id/files", s.listTransferJobFiles)
	v1.GET("/transfer-jobs/:id/logs", s.getTransferJobLogs)
	v1.POST("/transfer-jobs/:id/cancel", s.cancelTransferJob)
	v1.POST("/transfer-jobs/:id/retry", s.retryTransferJob)

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
	r.POST("/rules/ignore_errors", s.ruleIgnoreErrorsPost)
	r.POST("/rules/restore_errors", s.ruleRestoreErrorsPost)

	r.GET("/limit_groups", s.limitGroupsList)
	r.POST("/limit_groups/save", s.limitGroupsSavePost)
	r.POST("/limit_groups/delete", s.limitGroupsDeletePost)

	r.GET("/extension_presets", s.extensionPresetsList)
	r.POST("/extension_presets/save", s.extensionPresetsSavePost)
	r.POST("/extension_presets/delete", s.extensionPresetsDeletePost)

	r.GET("/manual", s.manualGet)
	r.POST("/manual/start", s.manualStartPost)

	r.GET("/jobs", s.jobsList)
	r.GET("/jobs/view", s.jobView)
	r.POST("/jobs/terminate", s.jobTerminatePost)
	r.POST("/jobs/retry", s.jobRetryPost)
	r.GET("/api/job/files", s.apiJobFiles)
	r.GET("/api/job", s.apiJob)
	r.GET("/api/job/log/stream", s.apiJobLogStream)
	r.GET("/api/job/transfers", s.apiJobTransfers)

	r.GET("/api/fs/list", s.apiFSList)
	r.GET("/api/rclone/dirs", s.apiRcloneDirs)

	r.GET("/api/stats/now", s.apiStatsNow)

	r.GET("/logs", s.logsPage)
	r.GET("/api/log/daemon/stream", s.apiDaemonLogStream)

	r.GET("/api-access", s.apiAccessPage)
	r.POST("/api-access/tokens/create", s.apiTokenCreatePost)
	r.POST("/api-access/tokens/toggle", s.apiTokenTogglePost)
	r.POST("/api-access/tokens/delete", s.apiTokenDeletePost)
	r.POST("/api-access/callback-secret/rotate", s.callbackSecretRotatePost)
	r.POST("/api-access/callback-secret/clear", s.callbackSecretClearPost)

	r.GET("/settings", s.settingsGet)
	r.POST("/settings/save", s.settingsSavePost)
	r.GET("/api/rclone/check", s.apiRcloneCheck)

	r.StaticFS("/static", http.FS(staticFS))

	return r
}

func (s *Server) render(c *gin.Context, name string, data any) {
	c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if m, ok := data.(map[string]any); ok {
		m["IsLogin"] = name == "login"
		if _, exists := m["PageTitle"]; !exists {
			m["PageTitle"] = pageTitles[name]
		}
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

func (s *Server) remotesList(c *gin.Context) {
	ctx := c.Request.Context()
	remotes, err := s.listRcloneRemotes(ctx)
	s.render(c, "remotes", map[string]any{
		"Active":  "remotes",
		"Remotes": remotes,
		"Error":   errString(err),
	})
}

func (s *Server) limitGroupsList(c *gin.Context) {
	ctx := c.Request.Context()
	groups, _ := s.st.ListLimitGroups(ctx)
	rules, _ := s.st.ListRules(ctx)

	// Map group -> []ruleID for JS pre-filling
	groupRulesMap := map[string][]string{}
	for _, r := range rules {
		if r.LimitGroup != "" {
			groupRulesMap[r.LimitGroup] = append(groupRulesMap[r.LimitGroup], r.ID)
		}
	}

	s.render(c, "limit_groups", map[string]any{
		"Active":        "limit_groups",
		"Groups":        groups,
		"Rules":         rules,
		"GroupRulesMap": groupRulesMap,
	})
}

func (s *Server) limitGroupsSavePost(c *gin.Context) {
	ctx := c.Request.Context()
	limit, err := parseSizeBytes(c.PostForm("daily_limit"))
	if err != nil {
		c.String(http.StatusBadRequest, "流量限制格式错误：%v", err)
		return
	}
	name := strings.TrimSpace(c.PostForm("name"))
	g := store.LimitGroup{
		Name:            name,
		DailyLimitBytes: limit,
	}
	if err := s.st.UpsertLimitGroup(ctx, g); err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}

	// Update associated rules
	ruleIDs := c.PostFormArray("rule_ids")
	if err := s.st.SetRulesForLimitGroup(ctx, name, ruleIDs); err != nil {
		log.Printf("failed to update rules for group %s: %v", name, err)
		uiError(c, http.StatusConflict, "", "分组限额已保存，但关联规则未能更新，请刷新检查")
		return
	}

	s.uiSuccess(c, "限流分组已保存", "/limit_groups")
}

func (s *Server) limitGroupsDeletePost(c *gin.Context) {
	ctx := c.Request.Context()
	if err := s.st.DeleteLimitGroup(ctx, c.PostForm("name")); err != nil {
		uiError(c, 400, "", "删除分组失败")
		return
	}
	s.uiSuccess(c, "分组已删除，关联规则恢复独立限额", "/limit_groups")
}

func (s *Server) extensionPresetsList(c *gin.Context) {
	ctx := c.Request.Context()
	presets, _ := s.st.ListExtensionPresets(ctx)
	s.render(c, "extension_presets", map[string]any{
		"Active":  "extension_presets",
		"Presets": presets,
	})
}

func (s *Server) extensionPresetsSavePost(c *gin.Context) {
	ctx := c.Request.Context()
	name := strings.TrimSpace(c.PostForm("name"))
	exts := strings.TrimSpace(c.PostForm("extensions"))
	if name == "" {
		c.String(http.StatusBadRequest, "名称不能为空")
		return
	}
	p := store.ExtensionPreset{
		Name:       name,
		Extensions: exts,
	}
	if err := s.st.UpsertExtensionPreset(ctx, p); err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}
	s.uiSuccess(c, "扩展名预设已保存", "/extension_presets")
}

func (s *Server) extensionPresetsDeletePost(c *gin.Context) {
	ctx := c.Request.Context()
	if err := s.st.DeleteExtensionPreset(ctx, c.PostForm("name")); err != nil {
		uiError(c, 400, "", "删除预设失败")
		return
	}
	s.uiSuccess(c, "扩展名预设已删除", "/extension_presets")
}

func (s *Server) manualGet(c *gin.Context) {
	ctx := c.Request.Context()
	copyFromID := strings.TrimSpace(c.Query("copy_from_id"))
	var rule store.Rule
	if copyFromID != "" {
		if got, ok, _ := s.st.GetRule(ctx, copyFromID); ok {
			rule = got
		}
	}
	// Defaults if empty
	if rule.TransferMode == "" {
		rule.TransferMode = "copy"
	}
	if rule.SrcKind == "" {
		rule.SrcKind = "remote"
	}

	remotes, err := s.listRcloneRemotes(ctx)
	rules, _ := s.st.ListRules(ctx)
	presets, _ := s.st.ListExtensionPresets(ctx)
	limitGroups, _ := s.st.ListLimitGroups(ctx)
	s.render(c, "manual", map[string]any{
		"Active":      "manual",
		"Manual":      true,
		"LimitGroups": limitGroups,
		"Remotes":     remotes,
		"Rule":        rule,
		"Rules":       rules,
		"Presets":     presets,
		"Error":       errString(err),
	})
}

func (s *Server) manualStartPost(c *gin.Context) {
	ctx := c.Request.Context()
	dailyLimit, err := parseSizeBytes(c.PostForm("daily_limit"))
	if err != nil {
		uiError(c, http.StatusBadRequest, "daily_limit", "流量限制格式错误")
		return
	}
	sourceSubpath, err := normalizeSubpath(c.PostForm("source_subpath"))
	if err != nil {
		uiError(c, http.StatusBadRequest, "source_subpath", "源子目录必须位于源根目录内")
		return
	}
	destinationSubpath, err := normalizeSubpath(c.PostForm("destination_subpath"))
	if err != nil {
		uiError(c, http.StatusBadRequest, "destination_subpath", "目标子目录必须位于目标根目录内")
		return
	}

	minSize, err := parseSizeBytes(c.PostForm("min_file_size"))
	if err != nil {
		uiError(c, http.StatusBadRequest, "min_file_size", "最小文件大小格式错误，请使用 10M、1.5G、0 或留空")
		return
	}

	if strings.TrimSpace(c.PostForm("rclone_extra_args")) != "" {
		if err := daemon.ValidateRcloneArgs(c.PostForm("rclone_extra_args")); err != nil {
			uiError(c, http.StatusBadRequest, "rclone_extra_args", err.Error())
			return
		}
	}

	jobID := newID()
	ruleID := "manual_" + jobID
	rule := store.Rule{
		ID:               ruleID,
		LimitGroup:       c.PostForm("limit_group"),
		DailyLimitBytes:  dailyLimit,
		GroupByDirectory: store.ParseEnabled(c.PostForm("group_by_directory")),
		AtomicPublish:    store.ParseEnabled(c.PostForm("atomic_publish")),
		StagingPath:      c.PostForm("staging_path"),
		SrcKind:          c.PostForm("src_kind"),
		SrcRemote:        c.PostForm("src_remote"),
		SrcPath:          c.PostForm("src_path"),
		SrcLocalRoot:     c.PostForm("src_local_root"),
		DstRemote:        c.PostForm("dst_remote"),
		DstPath:          c.PostForm("dst_path"),
		TransferMode:     c.PostForm("transfer_mode"),
		RcloneExtraArgs:  c.PostForm("rclone_extra_args"),
		ResumeEnabled:    store.ParseEnabled(c.PostForm("resume_enabled")),
		PartialDir:       strings.TrimSpace(c.PostForm("partial_dir")),
		PartialSuffix:    strings.TrimSpace(c.PostForm("partial_suffix")),
		IgnoreExtensions: c.PostForm("ignore_extensions"),
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

	frozen, _, err := s.st.GetRule(ctx, rule.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "load rule: %v", err)
		return
	}
	request, err := daemon.EncodeTransferSpec(daemon.TransferSpec{Operation: frozen.TransferMode, RuleSnapshot: &frozen, SourceSubpath: sourceSubpath, DestinationSubpath: destinationSubpath})
	if err != nil {
		c.String(http.StatusInternalServerError, "encode task: %v", err)
		return
	}
	job := store.TransferJob{JobID: jobID, RuleID: frozen.ID, Origin: store.OriginManual, TransferMode: frozen.TransferMode, RequestJSON: request, LogPath: logPath, QuotaGroup: frozen.LimitGroup}
	if err := s.st.CreateTransferJob(ctx, job, nil); err != nil {
		c.String(http.StatusInternalServerError, "create task: %v", err)
		return
	}
	s.uiSuccess(c, "任务已排队，正在准备本次文件清单", "/jobs/view?id="+jobID)
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
	case "pending", "blocked", "running", "done", "failed", "terminated":
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

func (s *Server) apiStatsNow(c *gin.Context) {
	ctx := c.Request.Context()
	ruleID := strings.TrimSpace(c.Query("rule_id"))
	sum, err := s.st.FreshRealtimeSummary(ctx, ruleID, s.metricFreshSince(ctx))
	globalSummary, _ := s.st.FreshRealtimeSummary(ctx, "", s.metricFreshSince(ctx))
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	bytesToday, _ := s.st.StatsBytesSince(ctx, todayStart)
	bytes24h, _ := s.st.StatsBytesSince(ctx, now.Add(-24*time.Hour))
	statusCounts, err := s.st.JobStatusCounts(ctx, store.JobFilter{})
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	attention, err := s.st.JobAttentionCount(ctx)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	statusCounts["attention"] = attention

	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(c.Writer).Encode(map[string]any{
		"ts":                time.Now().UnixMilli(),
		"ruleID":            ruleID,
		"bytesTotal":        sum.BytesTotal,
		"speedTotal":        sum.SpeedTotal,
		"runningJobs":       sum.RunningJobs,
		"globalSpeedTotal":  globalSummary.SpeedTotal,
		"globalRunningJobs": globalSummary.RunningJobs,
		"bytesToday":        bytesToday,
		"bytes24h":          bytes24h,
		"statusCounts":      statusCounts,
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
			"jobID":     job.JobID,
			"running":   false,
			"transfers": []any{},
		})
		return
	}
	transfers, source, err := fetchRcloneTransfers(ctx, job.RcPort)
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err != nil {
		_ = json.NewEncoder(c.Writer).Encode(map[string]any{
			"jobID":     job.JobID,
			"running":   true,
			"error":     err.Error(),
			"transfers": []any{},
		})
		return
	}
	_ = json.NewEncoder(c.Writer).Encode(map[string]any{
		"jobID":     job.JobID,
		"running":   true,
		"source":    source,
		"transfers": transfers,
	})
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
	if job.Status == "pending" || job.Status == "blocked" {
		if cancelled, err := s.st.CancelWaitingTask(ctx, id); err != nil || !cancelled {
			c.String(http.StatusConflict, "任务状态已变化，请刷新后重试")
			return
		}
	} else if job.Status != "running" || !s.supervisor.TerminateJob(id) {
		c.String(http.StatusConflict, "terminate failed: job not found in registry")
		return
	}
	next := safeNext(c.PostForm("next"), "/jobs")
	s.uiSuccess(c, "停止请求已提交，未完成内容可重试", next)
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

func humanSpeed(n float64) string {
	if n <= 0 {
		return "0 B/s"
	}
	return humanBytes(int64(n+0.5)) + "/s"
}
