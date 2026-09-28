package server

import (
	"net/http"
	"strings"

	"115togd/internal/daemon"
	"github.com/gin-gonic/gin"
)

func (s *Server) jobRetryPost(c *gin.Context) {
	ctx := c.Request.Context()
	source, ok, err := s.st.GetTransferJob(ctx, strings.TrimSpace(c.PostForm("id")))
	if err != nil || !ok {
		c.String(http.StatusNotFound, "任务不存在")
		return
	}
	job, err := daemon.RetryTask(ctx, s.st, source, newID(), "")
	if err != nil {
		c.String(http.StatusConflict, "重试失败：%v", err)
		return
	}
	s.redirect(c, "/jobs/view?id="+job.JobID)
}
func (s *Server) apiJobFiles(c *gin.Context) {
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.Query("id"))
	if _, ok, err := s.st.GetJob(ctx, id); err != nil || !ok {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	files, err := s.st.ListTransferJobFiles(ctx, id, c.Query("after"), 200)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	counts, err := s.st.TransferJobFileCounts(ctx, id)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	next := ""
	if len(files) == 200 {
		next = files[len(files)-1].Path
	}
	c.JSON(http.StatusOK, gin.H{"files": files, "counts": counts, "next": next})
}
