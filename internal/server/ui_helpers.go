package server

import (
	"bytes"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"115togd/internal/store"
	"github.com/gin-gonic/gin"
)

var pageTitles = map[string]string{
	"dashboard": "同步概览", "rules": "同步规则", "rule_edit": "规则配置",
	"jobs": "传输任务", "job_view": "任务详情", "manual": "手动运行",
	"remotes": "远程连接", "rclone_config": "rclone 配置", "logs": "系统日志",
	"settings": "系统设置", "limit_groups": "限流分组", "extension_presets": "扩展名预设",
	"api_access": "API 访问", "login": "登录",
}

func uiTemplateFuncs() template.FuncMap {
	icons := strings.Fields("sync dashboard tasks cloud settings file key log plus search refresh edit more pause play stop check close copy arrow-right arrow-left chevron-right chevron-left chevron-down sort filter sun moon menu logout info warning download folder scan shield")
	known := make(map[string]bool, len(icons))
	for _, name := range icons {
		known[name] = true
	}
	return template.FuncMap{
		"icon": func(name string) template.HTML {
			if !known[name] {
				name = "info"
			}
			return template.HTML("<svg class='app-icon' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='1.8' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><use href='/static/icons.svg#" + name + "'></use></svg>")
		},
		"statusName": statusName, "statusClass": statusClass, "phaseName": phaseName,
		"modeName": modeName, "sourcePath": sourcePath, "destinationPath": destinationPath,
		"relativeTime": relativeTime, "queryEscape": url.QueryEscape,
		"shortID": func(id string) string {
			if len(id) > 8 {
				return id[:8]
			}
			return id
		},
		"percent": func(used, total int64) float64 {
			if total <= 0 {
				return 0
			}
			return min(100.0, max(0.0, float64(used)*100/float64(total)))
		},
	}
}

func statusName(status string) string {
	switch status {
	case "pending":
		return "等待执行"
	case "blocked":
		return "等待条件恢复"
	case "running":
		return "执行中"
	case "done":
		return "已完成"
	case "failed":
		return "失败"
	case "terminated":
		return "已停止"
	case "paused":
		return "调度已暂停"
	case "idle":
		return "空闲"
	default:
		return "状态待同步"
	}
}

func statusClass(status string) string {
	switch status {
	case "pending", "blocked", "running", "done", "failed", "terminated", "paused", "idle":
		return "status status-" + status
	default:
		return "status status-idle"
	}
}

func phaseName(phase string) string {
	switch phase {
	case "preparing":
		return "准备清单"
	case "copying":
		return "传输中"
	case "verifying":
		return "整组校验"
	case "publishing":
		return "发布目录"
	case "cleanup":
		return "清理源文件"
	case "completed":
		return "完成"
	default:
		return ""
	}
}

func modeName(mode string) string {
	if mode == "move" {
		return "移动"
	}
	return "复制"
}

func sourcePath(rule store.Rule) string {
	if rule.SrcKind == "local" {
		return rule.SrcLocalRoot
	}
	if rule.SrcRemote == "" {
		return ""
	}
	return rule.SrcRemote + ":" + rule.SrcPath
}

func destinationPath(rule store.Rule) string {
	if rule.DstRemote == "" {
		return ""
	}
	return rule.DstRemote + ":" + rule.DstPath
}

func relativeTime(at time.Time) string {
	if at.IsZero() {
		return "尚未执行"
	}
	d := time.Since(at)
	if d < 5*time.Second {
		return "刚刚"
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + " 秒前"
	}
	if d < time.Hour {
		return strconv.Itoa(int(d.Minutes())) + " 分钟前"
	}
	if d < 24*time.Hour {
		return strconv.Itoa(int(d.Hours())) + " 小时前"
	}
	return at.Format("01-02 15:04")
}

func wantsUIJSON(c *gin.Context) bool {
	return c.GetHeader("X-Requested-With") == "XMLHttpRequest" || strings.Contains(c.GetHeader("Accept"), "application/json")
}

func (s *Server) uiSuccess(c *gin.Context, message, next string) {
	if wantsUIJSON(c) {
		c.JSON(http.StatusOK, gin.H{"message": message, "next": safeNext(next, "/")})
		return
	}
	s.redirect(c, next)
}

func uiError(c *gin.Context, status int, field, message string) {
	if wantsUIJSON(c) {
		c.JSON(status, gin.H{"message": message, "field": field})
		return
	}
	c.String(status, "%s", message)
}

func cleanPageURL(c *gin.Context) string {
	q := c.Request.URL.Query()
	for _, key := range []string{"partial", "notice", "error"} {
		q.Del(key)
	}
	u := url.URL{Path: c.Request.URL.Path, RawQuery: q.Encode()}
	return u.String()
}

func (s *Server) renderList(c *gin.Context, name, fragment string, data map[string]any) {
	if c.Query("partial") != "1" {
		s.render(c, name, data)
		return
	}
	var out bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&out, fragment, data); err != nil {
		uiError(c, http.StatusInternalServerError, "", "暂时无法更新列表")
		return
	}
	c.JSON(http.StatusOK, gin.H{"html": out.String(), "updated_at": time.Now().UnixMilli(), "page": data["Page"]})
}
