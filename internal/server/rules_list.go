package server

import (
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type ruleDisplayRow struct {
	ruleListRow
	Speed       float64
	Phase       string
	Status      string
	StatusLabel string
	Message     string
}

func (s *Server) rulesList(c *gin.Context) {
	ctx := c.Request.Context()
	rules, err := s.st.ListRules(ctx)
	if err != nil {
		uiError(c, 500, "", "读取规则失败")
		return
	}
	counts, err := s.st.RuleCountsAll(ctx)
	if err != nil {
		uiError(c, 500, "", "读取文件状态失败")
		return
	}
	activities, err := s.st.RuleActivities(ctx)
	if err != nil {
		uiError(c, 500, "", "读取任务状态失败")
		return
	}
	runtimes, err := s.st.RuleRuntimes(ctx)
	if err != nil {
		uiError(c, 500, "", "读取扫描状态失败")
		return
	}
	transfers, err := s.st.RuleTransferActivities(ctx, s.metricFreshSince(ctx))
	if err != nil {
		uiError(c, 500, "", "读取传输状态失败")
		return
	}
	usage, err := s.st.RuleUsageTotals(ctx, now24h())
	if err != nil {
		uiError(c, 500, "", "读取流量失败")
		return
	}
	q := strings.TrimSpace(c.Query("q"))
	enable := c.Query("enable")
	runtime := c.Query("runtime")
	group := c.Query("group")
	summary := map[string]int{}
	var rows []ruleDisplayRow
	for _, r := range rules {
		if enable == "enabled" && !r.Enabled || enable == "paused" && r.Enabled {
			continue
		}
		if group != "" && r.LimitGroup != group {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(r.ID+" "+sourcePath(r)+" "+destinationPath(r)), strings.ToLower(q)) {
			continue
		}
		count, a, run, tr := counts[r.ID], activities[r.ID], runtimes[r.ID], transfers[r.ID]
		failed := count.Failed > 0 || run.ScanError != "" || tr.Failed > 0
		blocked := run.BlockReason != "" || tr.Blocked > 0
		queued := a.Pending > 0 || count.Queued > 0
		idle := a.Running == 0 && !queued
		summary["all"]++
		if a.Running > 0 {
			summary["running"]++
		}
		if queued {
			summary["queued"]++
		}
		if failed {
			summary["failed"]++
		}
		if blocked {
			summary["blocked"]++
		}
		if !r.Enabled {
			summary["paused"]++
		}
		if idle {
			summary["idle"]++
		}
		match := runtime == "" || runtime == "running" && a.Running > 0 || runtime == "queued" && queued || runtime == "failed" && failed || runtime == "blocked" && blocked || runtime == "idle" && idle || runtime == "paused" && !r.Enabled
		if !match {
			continue
		}
		row := ruleDisplayRow{ruleListRow: ruleListRow{Rule: r, Counts: count, Usage24h: usage[r.ID], Runtime: run, Activity: a}, Speed: tr.Speed, Phase: tr.Phase, Status: "idle", StatusLabel: "空闲"}
		switch {
		case a.Running > 0:
			row.Status = "running"
			row.StatusLabel = phaseName(tr.Phase)
			if row.StatusLabel == "" {
				row.StatusLabel = "执行中"
			}
		case run.ScanStartedAt.After(run.ScanEndedAt):
			row.Status = "running"
			row.StatusLabel = "扫描中"
		case failed:
			row.Status = "failed"
			row.StatusLabel = "需要处理"
		case blocked:
			row.Status = "blocked"
			row.StatusLabel = blockLabel(run.BlockReason)
		case queued:
			row.Status = "pending"
			row.StatusLabel = "等待执行"
		}
		row.Message = run.ScanError
		if row.Message == "" {
			row.Message = run.BlockMessage
		}
		if row.Message == "" {
			row.Message = tr.LatestError
		}
		rows = append(rows, row)
	}
	sortBy := c.DefaultQuery("sort", "name")
	direction := c.DefaultQuery("direction", "asc")
	if direction != "desc" {
		direction = "asc"
	}
	switch sortBy {
	case "name", "speed", "usage", "scan":
	default:
		sortBy = "name"
	}
	sort.SliceStable(rows, func(i, j int) bool {
		comparison := strings.Compare(rows[i].Rule.ID, rows[j].Rule.ID)
		switch sortBy {
		case "speed":
			if rows[i].Speed != rows[j].Speed {
				if rows[i].Speed < rows[j].Speed {
					comparison = -1
				} else {
					comparison = 1
				}
			}
		case "usage":
			if rows[i].Usage24h != rows[j].Usage24h {
				if rows[i].Usage24h < rows[j].Usage24h {
					comparison = -1
				} else {
					comparison = 1
				}
			}
		case "scan":
			if !rows[i].Runtime.ScanEndedAt.Equal(rows[j].Runtime.ScanEndedAt) {
				if rows[i].Runtime.ScanEndedAt.Before(rows[j].Runtime.ScanEndedAt) {
					comparison = -1
				} else {
					comparison = 1
				}
			}
		}
		if direction == "desc" {
			return comparison > 0
		}
		return comparison < 0
	})
	total := len(rows)
	size := normalizePageSize(c.Query("page_size"), 20)
	pages := max(1, (total+size-1)/size)
	page := min(pages, max(1, atoiDefault(c.Query("page"), 1)))
	start := min(total, (page-1)*size)
	rows = rows[start:min(total, start+size)]
	groups, _ := s.st.ListLimitGroups(ctx)
	link := func(p int) string {
		return updateQuery(cleanPageURL(c), map[string]string{"page": strconv.Itoa(p), "page_size": strconv.Itoa(size)})
	}
	s.renderList(c, "rules", "rules_live", map[string]any{
		"Active": "rules", "Rules": rows, "Groups": groups, "Q": q, "Enable": enable, "RuntimeFilter": runtime, "GroupFilter": group,
		"Summary": summary, "Sort": sortBy, "Direction": direction, "SelfURL": cleanPageURL(c), "Page": page, "PageSize": size,
		"Total": total, "TotalPages": pages, "HasPrev": page > 1, "HasNext": page < pages, "PrevURL": link(max(1, page-1)), "NextURL": link(min(pages, page+1)),
		"Notice": c.Query("notice"), "IsError": c.Query("error") == "1",
	})
}
