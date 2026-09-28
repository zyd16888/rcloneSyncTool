package server

import (
	"time"

	"115togd/internal/store"
	"github.com/gin-gonic/gin"
)

type quotaView struct {
	Name         string
	Usage, Limit int64
}

func (s *Server) dashboard(c *gin.Context) {
	ctx := c.Request.Context()
	counts, err := s.st.JobStatusCounts(ctx, store.JobFilter{})
	if err != nil {
		uiError(c, 500, "", "读取概览失败")
		return
	}
	attention, err := s.st.JobAttentionCount(ctx)
	if err != nil {
		uiError(c, 500, "", "读取失败提示失败")
		return
	}
	counts["attention"] = attention
	all, _ := s.st.UIJobs(ctx, store.JobFilter{}, 10, 0, "created", "desc", s.metricFreshSince(ctx))
	views, err := s.jobViews(ctx, all)
	if err != nil {
		uiError(c, 500, "", "读取近期任务失败")
		return
	}
	rules, _ := s.st.ListRules(ctx)
	activity, _ := s.st.RuleTransferActivities(ctx, s.metricFreshSince(ctx))
	var speed float64
	for _, a := range activity {
		speed += a.Speed
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	bytesToday, _ := s.st.StatsBytesSince(ctx, today)
	bytes24h, _ := s.st.StatsBytesSince(ctx, now.Add(-24*time.Hour))
	totalBytes, _ := s.st.TotalBytesDone(ctx)
	groups, _ := s.st.ListLimitGroups(ctx)
	var quotas []quotaView
	for _, g := range groups {
		usage, _ := s.st.GroupUsageSince(ctx, g.Name, now.Add(-24*time.Hour))
		quotas = append(quotas, quotaView{Name: g.Name, Usage: usage, Limit: g.DailyLimitBytes})
	}
	s.renderList(c, "dashboard", "dashboard_live", map[string]any{"Active": "dashboard", "Summary": counts, "Jobs": views, "Rules": rules, "TotalSpeed": speed,
		"BytesToday": bytesToday, "Bytes24h": bytes24h, "TotalBytes": totalBytes, "LimitGroups": quotas})
}
