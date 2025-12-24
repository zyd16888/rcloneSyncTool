package server

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/gin-gonic/gin"
)

func rcloneInstalled() (bool, string) {
	p, err := exec.LookPath("rclone")
	if err != nil {
		return false, ""
	}
	return true, p
}

func (s *Server) injectBase(c *gin.Context, m map[string]any) {
	ok, path := rcloneInstalled()
	m["RcloneInstalled"] = ok
	m["RclonePath"] = path

	rs, err := s.st.RuntimeSettings(c.Request.Context())
	if err == nil {
		m["RcloneConfigPath"] = rs.RcloneConfigPath
		if strings.TrimSpace(rs.RcloneConfigPath) == "" {
			m["RcloneConfigPathDisplay"] = "（使用 rclone 默认配置路径）"
			m["RcloneConfigMissing"] = false
		} else {
			m["RcloneConfigPathDisplay"] = rs.RcloneConfigPath
			_, statErr := os.Stat(rs.RcloneConfigPath)
			m["RcloneConfigMissing"] = statErr != nil
		}
	}
}

func (s *Server) listRcloneRemotes(ctx context.Context) ([]string, error) {
	ok, _ := rcloneInstalled()
	if !ok {
		return nil, errors.New("未检测到 rclone，请先安装并确保 rclone 在 PATH 中")
	}
	rs, err := s.st.RuntimeSettings(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"listremotes"}
	if strings.TrimSpace(rs.RcloneConfigPath) != "" {
		if _, err := os.Stat(rs.RcloneConfigPath); err != nil {
			return nil, errors.New("rclone 配置文件不存在：" + rs.RcloneConfigPath)
		}
		args = append(args, "--config", rs.RcloneConfigPath)
	}
	cmd := exec.CommandContext(ctx, "rclone", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New("rclone listremotes 失败：" + msg)
	}
	var remotes []string
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimSuffix(line, ":")
		remotes = append(remotes, line)
	}
	return remotes, nil
}

func (s *Server) rcloneVersion(ctx context.Context) (string, error) {
	ok, _ := rcloneInstalled()
	if !ok {
		return "", errors.New("未检测到 rclone")
	}
	cmd := exec.CommandContext(ctx, "rclone", "version")
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	if err := cmd.Run(); err != nil {
		return "", errors.New(strings.TrimSpace(b.String()))
	}
	lines := strings.Split(strings.ReplaceAll(b.String(), "\r\n", "\n"), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}
