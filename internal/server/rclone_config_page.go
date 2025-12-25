package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const maxRcloneConfigBytes = 2 << 20 // 2MiB

func (s *Server) effectiveRcloneConfigPath(ctx context.Context) (string, string, error) {
	rs, err := s.st.RuntimeSettings(ctx)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(rs.RcloneConfigPath) != "" {
		return rs.RcloneConfigPath, "settings", nil
	}
	if p := strings.TrimSpace(os.Getenv("RCLONE_CONFIG")); p != "" {
		return p, "env", nil
	}
	out, err := s.rcloneCmdOutput(ctx, "config", "file")
	if err != nil {
		return "", "", err
	}
	// Example:
	// Configuration file is stored at:
	// /home/user/.config/rclone/rclone.conf
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		p := strings.TrimSpace(lines[i])
		if p == "" {
			continue
		}
		if strings.Contains(strings.ToLower(p), "configuration") {
			continue
		}
		return p, "rclone-default", nil
	}
	return "", "", errors.New("无法解析 rclone config file 输出")
}

func (s *Server) rcloneConfigGet(c *gin.Context) {
	ctx := c.Request.Context()

	ok, _ := rcloneInstalled()
	if !ok {
		s.render(c, "rclone_config", map[string]any{
			"Active": "rclone_config",
			"Error":  "未检测到 rclone，请先安装或使用 Docker 镜像自带的 rclone。",
		})
		return
	}

	p, source, err := s.effectiveRcloneConfigPath(ctx)
	if err != nil {
		s.render(c, "rclone_config", map[string]any{
			"Active": "rclone_config",
			"Error":  err.Error(),
		})
		return
	}

	content := ""
	var readErr string
	if strings.TrimSpace(p) != "" {
		if st, err := os.Stat(p); err == nil && st.Size() > maxRcloneConfigBytes {
			readErr = "配置文件过大，已拒绝加载（>2MiB）"
		} else {
			b, err := os.ReadFile(p)
			if err != nil {
				readErr = err.Error()
			} else if len(b) > maxRcloneConfigBytes {
				readErr = "配置文件过大，已拒绝加载（>2MiB）"
			} else {
				content = string(b)
			}
		}
	}

	s.render(c, "rclone_config", map[string]any{
		"Active":      "rclone_config",
		"Path":        p,
		"PathSource":  source,
		"Content":     content,
		"ReadError":   readErr,
		"HasContent":  content != "",
		"HasReadError": readErr != "",
	})
}

func (s *Server) rcloneConfigSavePost(c *gin.Context) {
	ctx := c.Request.Context()
	p, _, err := s.effectiveRcloneConfigPath(ctx)
	if err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}
	p = strings.TrimSpace(p)
	if p == "" {
		c.String(http.StatusBadRequest, "无法确定 rclone 配置文件路径")
		return
	}

	info, err := os.Stat(p)
	if err != nil {
		c.String(http.StatusBadRequest, "配置文件不存在，请先创建/挂载该文件：%v", err)
		return
	}
	if !info.Mode().IsRegular() {
		c.String(http.StatusBadRequest, "目标不是普通文件：%s", p)
		return
	}

	raw := c.PostForm("content")
	if len(raw) > maxRcloneConfigBytes {
		c.String(http.StatusBadRequest, "内容过大，已拒绝保存（>2MiB）")
		return
	}

	// Normalize line endings to \n to avoid platform issues.
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	out := []byte(normalized)

	dir := filepath.Dir(p)
	tmp := filepath.Join(dir, "."+filepath.Base(p)+".tmp."+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.WriteFile(tmp, out, info.Mode().Perm()); err != nil {
		c.String(http.StatusInternalServerError, "写入临时文件失败：%v", err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		c.String(http.StatusInternalServerError, "保存失败：%v", err)
		return
	}

	s.redirect(c, "/rclone/config")
}
