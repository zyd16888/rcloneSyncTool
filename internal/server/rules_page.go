package server

import (
	"115togd/internal/daemon"
	"115togd/internal/store"
	"fmt"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ruleListRow struct {
	Rule     store.Rule
	Counts   store.FileStateCounts
	Usage24h int64
	Runtime  store.RuleRuntime
	Activity store.RuleActivity
}

func now24h() time.Time { return time.Now().Add(-24 * time.Hour) }
func (s *Server) ruleFeedback(c *gin.Context, message string, isError bool) {
	if wantsUIJSON(c) {
		if isError {
			uiError(c, http.StatusBadRequest, "", message)
		} else {
			s.uiSuccess(c, message, safeNext(c.PostForm("return_url"), "/rules"))
		}
		return
	}
	next := safeNext(c.PostForm("return_url"), "/rules")
	u, err := url.Parse(next)
	if err != nil {
		u = &url.URL{Path: "/rules"}
	}
	values := u.Query()
	values.Set("notice", message)
	if isError {
		values.Set("error", "1")
	} else {
		values.Del("error")
	}
	u.RawQuery = values.Encode()
	s.redirect(c, u.String())
}
func (s *Server) ruleEditGet(c *gin.Context) {
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.Query("id"))
	copyFromID := strings.TrimSpace(c.Query("copy_from_id"))

	var rule store.Rule
	if id != "" {
		if got, ok, _ := s.st.GetRule(ctx, id); ok {
			rule = got
		}
	} else if copyFromID != "" {
		if got, ok, _ := s.st.GetRule(ctx, copyFromID); ok {
			rule = got
			rule.ID = ""         // Force new ID
			rule.Enabled = false // Default to disabled for safety
		}
	}

	if rule.ID == "" && copyFromID == "" { // Only apply defaults if not copying
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
	rules, _ := s.st.ListRules(ctx)
	limitGroups, _ := s.st.ListLimitGroups(ctx)
	presets, _ := s.st.ListExtensionPresets(ctx)
	s.render(c, "rule_edit", map[string]any{
		"Active":      "rules",
		"Rule":        rule,
		"Remotes":     remotes,
		"Rules":       rules,
		"LimitGroups": limitGroups,
		"Presets":     presets,
		"Error":       errString(err),
	})
}

func (s *Server) ruleSavePost(c *gin.Context) {
	ctx := c.Request.Context()
	minSize, err := parseSizeBytes(c.PostForm("min_file_size"))
	if err != nil {
		uiError(c, http.StatusBadRequest, "min_file_size", "最小文件大小格式错误，请使用 10M、1.5G、0 或留空")
		return
	}
	dailyLimit, err := parseSizeBytes(c.PostForm("daily_limit"))
	if err != nil {
		uiError(c, http.StatusBadRequest, "daily_limit", "流量限制格式错误，请使用 750G、0 或留空")
		return
	}
	if strings.TrimSpace(c.PostForm("rclone_extra_args")) != "" {
		if err := daemon.ValidateRcloneArgs(c.PostForm("rclone_extra_args")); err != nil {
			uiError(c, http.StatusBadRequest, "rclone_extra_args", err.Error())
			return
		}
	}
	rule := store.Rule{
		ID:                   c.PostForm("id"),
		LimitGroup:           strings.TrimSpace(c.PostForm("limit_group")),
		SrcKind:              c.PostForm("src_kind"),
		SrcRemote:            c.PostForm("src_remote"),
		SrcPath:              c.PostForm("src_path"),
		SrcLocalRoot:         c.PostForm("src_local_root"),
		LocalWatch:           store.ParseEnabled(c.PostForm("local_watch_enabled")),
		DstRemote:            c.PostForm("dst_remote"),
		DstPath:              c.PostForm("dst_path"),
		TransferMode:         c.PostForm("transfer_mode"),
		GroupByDirectory:     store.ParseEnabled(c.PostForm("group_by_directory")),
		AtomicPublish:        store.ParseEnabled(c.PostForm("atomic_publish")),
		StagingPath:          strings.TrimSpace(c.PostForm("staging_path")),
		ReadyMarker:          strings.TrimSpace(c.PostForm("ready_marker")),
		RcloneExtraArgs:      c.PostForm("rclone_extra_args"),
		ResumeEnabled:        store.ParseEnabled(c.PostForm("resume_enabled")),
		PartialDir:           strings.TrimSpace(c.PostForm("partial_dir")),
		PartialSuffix:        strings.TrimSpace(c.PostForm("partial_suffix")),
		IgnoreExtensions:     c.PostForm("ignore_extensions"),
		Bwlimit:              c.PostForm("bwlimit"),
		DailyLimitBytes:      dailyLimit,
		MinFileSizeBytes:     minSize,
		APIEnabled:           store.ParseEnabled(c.PostForm("api_enabled")),
		APIAllowedOperations: strings.TrimSpace(c.PostForm("api_allowed_operations")),
		MaxParallelJobs:      atoiDefault(c.PostForm("max_parallel_jobs"), 1),
		ScanIntervalSec:      atoiDefault(c.PostForm("scan_interval_sec"), 15),
		StableSeconds:        atoiDefault(c.PostForm("stable_seconds"), 60),
		BatchSize:            atoiDefault(c.PostForm("batch_size"), 100),
		Enabled:              store.ParseEnabled(c.PostForm("enabled")),
	}
	if err := s.st.UpsertRule(ctx, rule); err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}
	if !rule.Enabled && s.supervisor != nil {
		s.supervisor.StopRule(rule.ID)
	}
	s.ruleFeedback(c, "规则已保存；启用规则将在下一次调度时扫描存量目录", false)
}

func (s *Server) ruleDeletePost(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.PostForm("id")
	if err := s.st.DeleteRule(ctx, id); err != nil {
		s.ruleFeedback(c, err.Error(), true)
		return
	}
	if s.supervisor != nil {
		s.supervisor.StopRule(id)
	}
	s.ruleFeedback(c, "规则已删除，历史任务及流量记录已保留", false)
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
	s.ruleFeedback(c, "规则调度开关已更新，运行中的任务继续完成", false)
}

func (s *Server) ruleScanPost(c *gin.Context) {
	id := c.PostForm("id")
	if s.supervisor == nil || !s.supervisor.TriggerScan(id) {
		s.ruleFeedback(c, "规则尚未启用或工作线程正在更新，启用后会自动扫描存量目录", true)
		return
	}
	s.ruleFeedback(c, "已请求扫描，扫描结果将在规则状态中更新", false)
}

func (s *Server) ruleRetryFailedPost(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.PostForm("id")
	if _, ok, err := s.st.GetRule(ctx, id); err != nil || !ok {
		s.ruleFeedback(c, "规则不存在", true)
		return
	}
	tasks, err := s.st.FailedRuleTasks(ctx, id)
	if err != nil {
		s.ruleFeedback(c, err.Error(), true)
		return
	}
	jobs := 0
	for _, task := range tasks {
		if _, err := daemon.RetryTask(ctx, s.st, task, newID(), ""); err == nil {
			jobs++
		}
	}
	n, err := s.st.RetryFailed(ctx, id, 10000)
	if err != nil {
		s.ruleFeedback(c, err.Error(), true)
		return
	}
	if s.supervisor != nil {
		s.supervisor.TriggerScan(id)
	}
	s.ruleFeedback(c, fmt.Sprintf("已排队 %d 个重试任务、恢复 %d 个失败文件；暂停规则需启用后执行", jobs, n), false)
}
