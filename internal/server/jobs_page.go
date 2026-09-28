package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"115togd/internal/store"
	"github.com/gin-gonic/gin"
)

func (s *Server) jobsList(c *gin.Context) {
	ctx := c.Request.Context()
	page := max(1, atoiDefault(c.Query("page"), 1))
	size := normalizePageSize(c.Query("page_size"), 20)
	filter := store.JobFilter{RuleID: strings.TrimSpace(c.Query("rule_id")), Status: normalizeJobStatus(c.Query("status")), TransferMode: normalizeTransferMode(c.Query("mode")), Query: strings.TrimSpace(c.Query("q"))}
	sortBy := c.DefaultQuery("sort", "created")
	switch sortBy {
	case "created", "name", "speed", "bytes", "status":
	default:
		sortBy = "created"
	}
	direction := c.DefaultQuery("direction", "desc")
	if direction != "asc" {
		direction = "desc"
	}
	total, err := s.st.CountJobsFiltered(ctx, filter)
	if err != nil {
		uiError(c, 500, "", "读取任务失败")
		return
	}
	pages := max(1, (total+size-1)/size)
	page = min(page, pages)
	jobs, err := s.st.UIJobs(ctx, filter, size, (page-1)*size, sortBy, direction, s.metricFreshSince(ctx))
	if err != nil {
		uiError(c, 500, "", "读取任务失败")
		return
	}
	views, err := s.jobViews(ctx, jobs)
	if err != nil {
		uiError(c, 500, "", "读取任务状态失败")
		return
	}
	counts, err := s.st.JobStatusCounts(ctx, filter)
	if err != nil {
		uiError(c, 500, "", "读取任务统计失败")
		return
	}
	rules, _ := s.st.ListRules(ctx)
	link := func(p int) string {
		q := c.Request.URL.Query()
		q.Del("partial")
		q.Set("page", strconv.Itoa(p))
		q.Set("page_size", strconv.Itoa(size))
		return "/jobs?" + q.Encode()
	}
	s.renderList(c, "jobs", "jobs_live", map[string]any{
		"Active": "jobs", "Jobs": views, "Rules": rules, "F": filter, "Sort": sortBy, "Direction": direction, "Summary": counts,
		"SelfURL": cleanPageURL(c), "Page": page, "PageSize": size, "Total": total, "TotalPages": pages,
		"HasPrev": page > 1, "HasNext": page < pages, "PrevURL": link(max(1, page-1)), "NextURL": link(min(pages, page+1)),
	})
}

func (s *Server) jobView(c *gin.Context) {
	ctx := c.Request.Context()
	job, ok, err := s.st.UIJob(ctx, strings.TrimSpace(c.Query("id")))
	if err != nil || !ok {
		uiError(c, http.StatusNotFound, "", "任务不存在")
		return
	}
	views, err := s.jobViews(ctx, []store.TransferJob{job})
	if err != nil {
		uiError(c, 500, "", "读取任务详情失败")
		return
	}
	data := map[string]any{"Active": "jobs", "View": views[0]}
	if c.Query("partial") == "1" {
		c.Header("Content-Type", "text/html; charset=utf-8")
		if err := s.pages["job_view"].ExecuteTemplate(c.Writer, "job_detail", data); err != nil {
			c.AbortWithStatus(500)
		}
		return
	}
	s.render(c, "job_view", data)
}

func (s *Server) apiJob(c *gin.Context) {
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.Query("id"))
	job, ok, err := s.st.GetJob(ctx, id)
	if err != nil || !ok {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	transfer, ok, err := s.st.UIJob(ctx, id)
	if err != nil || !ok {
		c.AbortWithStatus(500)
		return
	}
	views, err := s.jobViews(ctx, []store.TransferJob{transfer})
	if err != nil {
		c.AbortWithStatus(500)
		return
	}
	metric, hasMetric, _ := s.st.LatestJobMetric(ctx, id)
	v := views[0]
	c.JSON(200, gin.H{"job": job, "metric": metric, "hasMetric": hasMetric,
		"doneCount": v.FilesDone, "filesTotal": v.FilesTotal, "filesFailed": v.FilesFailed, "doneError": "", "view": v, "updated_at": time.Now().UnixMilli()})
}

func updateQuery(raw string, values map[string]string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, v := range values {
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
