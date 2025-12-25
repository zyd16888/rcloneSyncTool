package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

const maxDirSuggestions = 200

func hasTrailingSlash(p string) bool {
	if p == "" {
		return false
	}
	last := p[len(p)-1]
	return last == '/' || last == '\\'
}

func splitLocalDirPrefix(p string) (dir string, prefix string) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", ""
	}
	if hasTrailingSlash(p) {
		return p, ""
	}
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p, ""
	}
	return filepath.Dir(p), filepath.Base(p)
}

func normalizeRemotePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func splitRemoteDirPrefix(p string) (dir string, prefix string) {
	p = normalizeRemotePath(p)
	if p == "/" {
		return "/", ""
	}
	if strings.HasSuffix(p, "/") {
		return path.Clean(p), ""
	}
	d := path.Dir(p)
	if d == "." || d == "" {
		d = "/"
	}
	return d, path.Base(p)
}

func capSuggestions(in []string) (out []string, truncated bool) {
	out = in
	if len(out) > maxDirSuggestions {
		out = out[:maxDirSuggestions]
		truncated = true
	}
	return out, truncated
}

func (s *Server) apiFSList(c *gin.Context) {
	raw := strings.TrimSpace(c.Query("path"))
	if raw == "" {
		c.JSON(http.StatusOK, map[string]any{
			"suggestions": []string{},
			"truncated":   false,
		})
		return
	}

	dir, prefix := splitLocalDirPrefix(raw)
	if strings.TrimSpace(dir) == "" {
		c.JSON(http.StatusOK, map[string]any{
			"dir":         dir,
			"prefix":      prefix,
			"suggestions": []string{},
			"truncated":   false,
		})
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		c.JSON(http.StatusOK, map[string]any{
			"dir":         dir,
			"prefix":      prefix,
			"suggestions": []string{},
			"truncated":   false,
			"error":       err.Error(),
		})
		return
	}

	prefixLower := strings.ToLower(prefix)
	var suggestions []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := strings.TrimSpace(e.Name())
		if name == "" {
			continue
		}
		if prefixLower != "" && !strings.HasPrefix(strings.ToLower(name), prefixLower) {
			continue
		}
		suggestions = append(suggestions, filepath.Join(dir, name))
	}
	sort.Strings(suggestions)
	suggestions, truncated := capSuggestions(suggestions)

	c.JSON(http.StatusOK, map[string]any{
		"dir":         dir,
		"prefix":      prefix,
		"suggestions": suggestions,
		"truncated":   truncated,
	})
}

func (s *Server) apiRcloneDirs(c *gin.Context) {
	ctx := c.Request.Context()
	remote := strings.TrimSpace(c.Query("remote"))
	inPath := strings.TrimSpace(c.Query("path"))
	if remote == "" {
		c.JSON(http.StatusOK, map[string]any{
			"suggestions": []string{},
			"truncated":   false,
		})
		return
	}

	dir, prefix := splitRemoteDirPrefix(inPath)
	remoteSpec := fmt.Sprintf("%s:%s", remote, dir)

	out, err := s.rcloneCmdOutput(ctx, "lsf", remoteSpec, "--dirs-only", "--max-depth", "1")
	if err != nil {
		c.JSON(http.StatusOK, map[string]any{
			"remote":      remote,
			"dir":         dir,
			"prefix":      prefix,
			"suggestions": []string{},
			"truncated":   false,
			"error":       err.Error(),
		})
		return
	}

	prefixLower := strings.ToLower(prefix)
	var suggestions []string
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		name = strings.TrimSuffix(name, "/")
		if name == "" {
			continue
		}
		if prefixLower != "" && !strings.HasPrefix(strings.ToLower(name), prefixLower) {
			continue
		}
		full := path.Join(dir, name)
		if !strings.HasPrefix(full, "/") {
			full = "/" + full
		}
		suggestions = append(suggestions, full)
	}
	sort.Strings(suggestions)
	suggestions, truncated := capSuggestions(suggestions)

	c.JSON(http.StatusOK, map[string]any{
		"remote":      remote,
		"dir":         dir,
		"prefix":      prefix,
		"suggestions": suggestions,
		"truncated":   truncated,
	})
}

func (s *Server) rcloneCmdOutput(ctx context.Context, args ...string) ([]byte, error) {
	ok, _ := rcloneInstalled()
	if !ok {
		return nil, errors.New("未检测到 rclone，请先安装并确保 rclone 在 PATH 中")
	}
	rs, err := s.st.RuntimeSettings(ctx)
	if err != nil {
		return nil, err
	}

	allArgs := append([]string{}, args...)
	if strings.TrimSpace(rs.RcloneConfigPath) != "" {
		if _, err := os.Stat(rs.RcloneConfigPath); err != nil {
			return nil, errors.New("rclone 配置文件不存在：" + rs.RcloneConfigPath)
		}
		allArgs = append(allArgs, "--config", rs.RcloneConfigPath)
	}

	cmd := exec.CommandContext(ctx, "rclone", allArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New("rclone 调用失败：" + msg)
	}
	return out, nil
}
