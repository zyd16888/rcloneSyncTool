package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Server) logsPage(c *gin.Context) {
	c.Status(http.StatusOK)
	s.render(c, "logs", map[string]any{
		"Active":     "logs",
		"AppLogPath": s.appLogPath,
	})
}

