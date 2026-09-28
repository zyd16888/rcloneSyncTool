package server

import (
	"errors"
	"net/http"
	"strings"

	"115togd/internal/store"
	"github.com/gin-gonic/gin"
)

func (s *Server) ruleIgnoreErrorsPost(c *gin.Context) {
	s.setRuleErrorsIgnored(c, true)
}

func (s *Server) ruleRestoreErrorsPost(c *gin.Context) {
	s.setRuleErrorsIgnored(c, false)
}

func (s *Server) setRuleErrorsIgnored(c *gin.Context, ignored bool) {
	ids := c.PostFormArray("id")
	if len(ids) == 0 || len(ids) > 100 {
		uiError(c, http.StatusBadRequest, "", "请选择 1 至 100 条规则")
		return
	}
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
		if ids[i] == "" {
			uiError(c, http.StatusBadRequest, "", "规则 ID 不能为空")
			return
		}
	}
	changed, err := s.st.SetRuleErrorsIgnored(c.Request.Context(), ids, ignored)
	if err != nil {
		if errors.Is(err, store.ErrAttentionRuleNotFound) {
			uiError(c, http.StatusNotFound, "", "规则不存在或已删除")
		} else {
			uiError(c, http.StatusInternalServerError, "", "保存错误提示状态失败")
		}
		return
	}
	message := "已忽略当前错误提示，新的失败仍会提醒"
	if !ignored {
		message = "已恢复错误提示"
	} else if changed == 0 {
		message = "当前错误已处理或已忽略"
	}
	s.ruleFeedback(c, message, false)
}
