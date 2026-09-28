package server

import (
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) settingsGet(c *gin.Context) {
	ctx := c.Request.Context()
	all, _ := s.st.ListSettings(ctx)
	m := map[string]string{}
	for _, kv := range all {
		m[kv.Key] = kv.Value
	}
	s.render(c, "settings", map[string]any{
		"Active": "settings",
		"S":      m,
		"LogDir": s.logDir,
	})
}

func (s *Server) settingsSavePost(c *gin.Context) {
	ctx := c.Request.Context()
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
			c.String(http.StatusBadRequest, "设置 %s 必须在 %d 到 %d 之间", key, bounds[0], bounds[1])
			return
		}
		values[key] = strconv.Itoa(n)
	}
	current, err := s.st.RuntimeSettings(ctx)
	if err != nil {
		current.RcPortStart = 55720
		current.RcPortEnd = 55800
	}
	start, end := current.RcPortStart, current.RcPortEnd
	if v := values["rc_port_start"]; v != "" {
		start, _ = strconv.Atoi(v)
	}
	if v := values["rc_port_end"]; v != "" {
		end, _ = strconv.Atoi(v)
	}
	if end < start {
		c.String(http.StatusBadRequest, "RC 结束端口不能小于开始端口")
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
				c.String(http.StatusBadRequest, "设置 %s 大小格式错误：%v", key, err)
				return
			}
		}
	}
	passwordChanged := false
	if p := strings.TrimSpace(c.PostForm("ui_password")); p != "" {
		if p != strings.TrimSpace(c.PostForm("ui_password2")) {
			c.String(http.StatusBadRequest, "两次输入的密码不一致")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
		if err != nil {
			c.String(http.StatusBadRequest, "密码长度不受支持")
			return
		}
		values[authPasswordHashKey] = string(hash)
		passwordChanged = true
	}
	if err := s.st.SetSettings(ctx, values); err != nil {
		c.String(http.StatusInternalServerError, "保存设置失败：%v", err)
		return
	}
	if passwordChanged {
		clearAuthCookie(c)
		s.redirect(c, "/login?next=%2Fsettings")
		return
	}
	s.redirect(c, "/settings")
}
