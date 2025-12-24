package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Server) apiRcloneCheck(c *gin.Context) {
	ctx := c.Request.Context()

	ok, path := rcloneInstalled()
	resp := map[string]any{
		"installed": ok,
		"path":      path,
	}
	if !ok {
		resp["hint"] = "未检测到 rclone，请先安装并确保 rclone 在 PATH 中（重启终端后再试）"
		c.JSON(http.StatusOK, resp)
		return
	}

	rs, _ := s.st.RuntimeSettings(ctx)
	resp["configPath"] = rs.RcloneConfigPath
	resp["configPathDisplay"] = rs.RcloneConfigPath
	if rs.RcloneConfigPath == "" {
		resp["configPathDisplay"] = "（使用 rclone 默认配置路径）"
	}

	if v, err := s.rcloneVersion(ctx); err == nil {
		resp["version"] = v
	} else {
		resp["versionError"] = err.Error()
	}

	if remotes, err := s.listRcloneRemotes(ctx); err == nil {
		resp["remotes"] = remotes
		resp["remoteCount"] = len(remotes)
	} else {
		resp["remotesError"] = err.Error()
	}

	c.JSON(http.StatusOK, resp)
}

