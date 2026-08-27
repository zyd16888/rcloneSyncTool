package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"115togd/internal/store"
)

// The API access page manages the two credentials that let an external
// workflow drive this service: bearer tokens and the shared callback signing
// secret. Both are shown exactly once, at creation time, and only ever stored
// hashed or as configuration the operator already controls.
func (s *Server) apiAccessPage(c *gin.Context) {
	s.renderAPIAccess(c, nil)
}

// renderAPIAccess draws the page. `extra` carries a one-time reveal or an error
// for the request that produced it; a plaintext secret is rendered directly in
// the response rather than passed through a redirect, so it never lands in the
// browser history, a proxy log or a Referer header.
func (s *Server) renderAPIAccess(c *gin.Context, extra map[string]any) {
	ctx := c.Request.Context()
	tokens, err := s.st.ListAPITokens(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "list tokens: %v", err)
		return
	}
	secret, err := s.st.CallbackSecret(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "read callback secret: %v", err)
		return
	}
	rules, err := s.apiRules(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "list rules: %v", err)
		return
	}

	now := time.Now()
	views := make([]apiTokenView, 0, len(tokens))
	for _, token := range tokens {
		views = append(views, apiTokenView{
			Token:   token,
			Active:  token.Active(now),
			Expired: token.ExpiresAt > 0 && now.Unix() >= token.ExpiresAt,
		})
	}

	data := map[string]any{
		"Active":             "api_access",
		"Tokens":             views,
		"CallbackConfigured": secret != "",
		"CallbackLength":     len(secret),
		"APIRules":           rules,
	}
	for key, value := range extra {
		data[key] = value
	}
	s.render(c, "api_access", data)
}

// apiTokenView adds display-only state. Embedding by value keeps the template
// away from anything the store does not already expose.
type apiTokenView struct {
	Token   store.APIToken
	Active  bool
	Expired bool
}

func (t apiTokenView) ID() string    { return t.Token.ID }
func (t apiTokenView) Name() string  { return t.Token.Name }
func (t apiTokenView) Enabled() bool { return t.Token.Enabled }

func (t apiTokenView) CreatedText() string  { return formatUnixTime(t.Token.CreatedAt) }
func (t apiTokenView) ExpiresText() string  { return formatUnixTime(t.Token.ExpiresAt) }
func (t apiTokenView) LastUsedText() string { return formatUnixTime(t.Token.LastUsedAt) }

func formatUnixTime(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

func (s *Server) apiTokenCreatePost(c *gin.Context) {
	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" {
		s.renderAPIAccess(c, map[string]any{"Error": "请填写 Token 名称"})
		return
	}
	expiresAt := time.Time{}
	if days, err := strconv.Atoi(strings.TrimSpace(c.PostForm("expires_days"))); err == nil && days > 0 {
		expiresAt = time.Now().AddDate(0, 0, days)
	}
	token, plaintext, err := s.st.CreateAPIToken(c.Request.Context(), name, expiresAt)
	if err != nil {
		s.renderAPIAccess(c, map[string]any{"Error": "创建失败：" + err.Error()})
		return
	}
	s.renderAPIAccess(c, map[string]any{
		"NewToken":     plaintext,
		"NewTokenName": token.Name,
	})
}

func (s *Server) apiTokenTogglePost(c *gin.Context) {
	id := strings.TrimSpace(c.PostForm("id"))
	enabled := store.ParseEnabled(c.PostForm("enabled"))
	if err := s.st.SetAPITokenEnabled(c.Request.Context(), id, enabled); err != nil {
		s.renderAPIAccess(c, map[string]any{"Error": "更新失败：" + err.Error()})
		return
	}
	s.redirect(c, "/api-access")
}

func (s *Server) apiTokenDeletePost(c *gin.Context) {
	id := strings.TrimSpace(c.PostForm("id"))
	if err := s.st.DeleteAPIToken(c.Request.Context(), id); err != nil {
		s.renderAPIAccess(c, map[string]any{"Error": "删除失败：" + err.Error()})
		return
	}
	s.redirect(c, "/api-access")
}

func (s *Server) callbackSecretRotatePost(c *gin.Context) {
	secret, err := store.NewCallbackSecret()
	if err != nil {
		s.renderAPIAccess(c, map[string]any{"Error": "生成失败：" + err.Error()})
		return
	}
	if err := s.st.SetCallbackSecret(c.Request.Context(), secret); err != nil {
		s.renderAPIAccess(c, map[string]any{"Error": "保存失败：" + err.Error()})
		return
	}
	s.renderAPIAccess(c, map[string]any{"NewCallbackSecret": secret})
}

func (s *Server) callbackSecretClearPost(c *gin.Context) {
	if err := s.st.SetCallbackSecret(c.Request.Context(), ""); err != nil {
		s.renderAPIAccess(c, map[string]any{"Error": "清除失败：" + err.Error()})
		return
	}
	s.redirect(c, "/api-access")
}
