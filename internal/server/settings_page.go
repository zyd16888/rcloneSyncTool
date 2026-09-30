package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// Only return editable settings; stored credentials must never enter UI JSON.
func (s *Server) settingsFormValues(ctx context.Context) (map[string]string, error) {
	all, err := s.st.ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, kv := range all {
		switch kv.Key {
		case "rclone_config_path", "rclone_buffer_size", "rclone_drive_chunk_size", "rclone_bwlimit", "callback_allowed_hosts",
			"log_retention_days", "global_max_jobs", "rc_port_start", "rc_port_end", "rclone_transfers", "rclone_checkers",
			"metrics_interval_ms", "scheduler_tick_ms", "scan_timeout_sec":
			m[kv.Key] = kv.Value
		}
	}
	rs, err := s.st.RuntimeSettings(ctx)
	if err != nil {
		// Keep invalid stored values editable so the settings page can repair them.
		return m, nil
	}
	for key, value := range map[string]int{
		"log_retention_days": rs.LogRetentionDays, "global_max_jobs": rs.GlobalMaxJobs,
		"rc_port_start": rs.RcPortStart, "rc_port_end": rs.RcPortEnd,
		"rclone_transfers": rs.Transfers, "rclone_checkers": rs.Checkers,
		"metrics_interval_ms": int(rs.MetricsInterval / time.Millisecond),
		"scheduler_tick_ms":   int(rs.SchedulerTick / time.Millisecond), "scan_timeout_sec": int(rs.ScanTimeout / time.Second),
	} {
		m[key] = strconv.Itoa(value)
	}
	return m, nil
}

func (s *Server) settingsGet(c *gin.Context) {
	m, err := s.settingsFormValues(c.Request.Context())
	if err != nil {
		uiError(c, http.StatusInternalServerError, "", "读取系统设置失败")
		return
	}
	s.render(c, "settings", map[string]any{
		"Active": "settings",
		"S":      m,
		"LogDir": s.logDir,
	})
}

func (s *Server) settingsSavePost(c *gin.Context) {
	ctx := c.Request.Context()
	current, err := s.settingsFormValues(ctx)
	if err != nil {
		uiError(c, http.StatusInternalServerError, "", "读取系统设置失败，未保存更改")
		return
	}
	values := map[string]string{}
	ranges := map[string][2]int{
		"log_retention_days": {0, 3650}, "global_max_jobs": {0, 10000}, "rc_port_start": {1, 65535}, "rc_port_end": {1, 65535},
		"rclone_transfers": {1, 1000}, "rclone_checkers": {1, 1000}, "metrics_interval_ms": {100, 3600000}, "scheduler_tick_ms": {100, 3600000}, "scan_timeout_sec": {1, 86400},
	}
	for key, bounds := range ranges {
		raw := strings.TrimSpace(c.PostForm(key))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < bounds[0] || n > bounds[1] {
			uiError(c, http.StatusBadRequest, key, fmt.Sprintf("设置 %s 必须在 %d 到 %d 之间", key, bounds[0], bounds[1]))
			return
		}
		values[key] = strconv.Itoa(n)
	}
	start, _ := strconv.Atoi(current["rc_port_start"])
	end, _ := strconv.Atoi(current["rc_port_end"])
	if v := values["rc_port_start"]; v != "" {
		start, _ = strconv.Atoi(v)
	}
	if v := values["rc_port_end"]; v != "" {
		end, _ = strconv.Atoi(v)
	}
	if end < start {
		uiError(c, http.StatusBadRequest, "rc_port_end", "RC 结束端口不能小于开始端口")
		return
	}
	for _, key := range []string{"rclone_config_path", "rclone_buffer_size", "rclone_drive_chunk_size", "rclone_bwlimit", "callback_allowed_hosts"} {
		if v, exists := c.GetPostForm(key); exists {
			values[key] = strings.TrimSpace(v)
		}
	}
	for _, key := range []string{"rclone_buffer_size", "rclone_drive_chunk_size"} {
		if v := values[key]; v != "" {
			if _, err := parseSizeBytes(v); err != nil {
				uiError(c, http.StatusBadRequest, key, fmt.Sprintf("设置 %s 大小格式错误：%v", key, err))
				return
			}
		}
	}
	passwordChanged := false
	if p := strings.TrimSpace(c.PostForm("ui_password")); p != "" {
		if p != strings.TrimSpace(c.PostForm("ui_password2")) {
			uiError(c, http.StatusBadRequest, "ui_password2", "两次输入的密码不一致")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
		if err != nil {
			uiError(c, http.StatusBadRequest, "ui_password", "密码长度不受支持")
			return
		}
		values[authPasswordHashKey] = string(hash)
		passwordChanged = true
	}
	if err := s.st.SetSettings(ctx, values); err != nil {
		uiError(c, http.StatusInternalServerError, "", fmt.Sprintf("保存设置失败：%v", err))
		return
	}
	if passwordChanged {
		clearAuthCookie(c)
		s.uiSuccess(c, "设置已保存，请使用新密码重新登录", "/login?next=%2Fsettings")
		return
	}
	if wantsUIJSON(c) {
		for key, value := range values {
			current[key] = value
		}
		base := map[string]any{}
		s.injectBase(c, base)
		c.JSON(http.StatusOK, gin.H{"message": "系统设置已保存", "next": "/settings", "values": current, "config_path_display": base["RcloneConfigPathDisplay"]})
		return
	}
	s.uiSuccess(c, "系统设置已保存", "/settings")
}
