package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"115togd/internal/store"
	"115togd/internal/version"
)

// Limits advertised through /api/v1/capabilities. They are also the values the
// transfer job endpoints enforce, so a client can size a request before sending
// it instead of discovering the ceiling through a rejection.
const (
	apiMaxFilesPerJob    = 5000
	apiMaxSubpathBytes   = 1024
	apiMaxRequestBytes   = 256 * 1024
	apiRequestIDHeader   = "X-Request-Id"
	apiTokenContextKey   = "api_token"
	apiRequestIDCtxKey   = "api_request_id"
	rcloneVersionTimeout = 5 * time.Second
)

// apiErrorBody is the single error shape for every /api/v1 response. Clients
// branch on code, never on the human message.
type apiErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func apiFail(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": apiErrorBody{
			Code:      code,
			Message:   message,
			RequestID: apiRequestID(c),
		},
	})
}

func apiRequestID(c *gin.Context) string {
	if v, ok := c.Get(apiRequestIDCtxKey); ok {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

// apiTokenMiddleware authenticates /api/v1 with a bearer token. It is mounted
// before the cookie middleware so the browser session can never be used to
// drive machine endpoints, and a token can never open the Web UI.
func (s *Server) apiTokenMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := strings.TrimSpace(c.GetHeader(apiRequestIDHeader))
		if requestID == "" || len(requestID) > 128 {
			requestID = newID()
		}
		c.Set(apiRequestIDCtxKey, requestID)
		c.Writer.Header().Set(apiRequestIDHeader, requestID)

		presented, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			apiFail(c, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		token, ok, err := s.st.AuthenticateAPIToken(c.Request.Context(), presented)
		if err != nil {
			apiFail(c, http.StatusInternalServerError, "internal_error", "token lookup failed")
			return
		}
		if !ok {
			apiFail(c, http.StatusUnauthorized, "unauthorized", "invalid or disabled token")
			return
		}
		c.Set(apiTokenContextKey, token)
		c.Next()
	}
}

func bearerToken(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	value := strings.TrimSpace(header[len(prefix):])
	if value == "" {
		return "", false
	}
	return value, true
}

func (s *Server) apiV1Version(c *gin.Context) {
	payload := gin.H{
		"name":        version.Name,
		"app_version": version.AppVersion,
		"api_version": version.APIVersion,
		"commit":      version.Commit(),
		"build_time":  version.BuildTime(),
	}
	installed, _ := rcloneInstalled()
	payload["rclone_available"] = installed
	if installed {
		ctx, cancel := context.WithTimeout(c.Request.Context(), rcloneVersionTimeout)
		defer cancel()
		if v, err := s.rcloneVersion(ctx); err == nil {
			payload["rclone_version"] = v
		}
	}
	c.JSON(http.StatusOK, payload)
}

func (s *Server) apiV1Capabilities(c *gin.Context) {
	rules, err := s.apiRules(c.Request.Context())
	if err != nil {
		apiFail(c, http.StatusInternalServerError, "internal_error", "list rules failed")
		return
	}
	exposed := make([]gin.H, 0, len(rules))
	for _, rule := range rules {
		exposed = append(exposed, gin.H{
			"id":                 rule.ID,
			"src_kind":           rule.SrcKind,
			"src_remote":         rule.SrcRemote,
			"src_path":           rule.SrcPath,
			"src_local_root":     rule.SrcLocalRoot,
			"dst_remote":         rule.DstRemote,
			"dst_path":           rule.DstPath,
			"transfer_mode":      rule.TransferMode,
			"allowed_operations": rule.AllowedAPIOperations(),
			"limit_group":        rule.LimitGroup,
			"scheduler_enabled":  rule.Enabled,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"api_version": version.APIVersion,
		"operations":  []string{"copy", "move"},
		// Feature flags let a client detect exactly which stage of the
		// integration this deployment carries, instead of probing endpoints
		// and interpreting a 404 as "unsupported" or "misrouted".
		"features": gin.H{
			"transfer_jobs": true,
			"files_list":    true,
			"logs_cursor":   true,
			"cancel":        true,
			"retry":         true,
			"callback_hmac": true,
		},
		"limits": gin.H{
			"max_files":         apiMaxFilesPerJob,
			"max_subpath_bytes": apiMaxSubpathBytes,
			"max_request_bytes": apiMaxRequestBytes,
		},
		"rules": exposed,
	})
}

func (s *Server) apiV1Health(c *gin.Context) {
	ctx := c.Request.Context()
	status := "ok"

	installed, rclonePath := rcloneInstalled()
	if !installed {
		status = "degraded"
	}

	database := "ok"
	running, err := s.st.CountRunningJobsAll(ctx)
	if err != nil {
		database = "error"
		status = "degraded"
	}

	rules, err := s.apiRules(ctx)
	if err != nil {
		database = "error"
		status = "degraded"
	}
	if len(rules) == 0 && status == "ok" {
		// A reachable service with no API-enabled rule cannot accept work.
		// Saying so here turns a later "rule_not_found" into a setup step.
		status = "degraded"
	}

	c.JSON(http.StatusOK, gin.H{
		"status": status,
		"rclone": gin.H{
			"available": installed,
			"path":      rclonePath,
		},
		"database":     database,
		"running_jobs": running,
		"api_rules":    len(rules),
		"checked_at":   time.Now().UTC().Format(time.RFC3339),
	})
}

// apiRules returns the rules an API caller may reference. Rules are opt-in:
// nothing is reachable through /api/v1 until an operator enables it, so opening
// the API never silently exposes every existing sync rule.
func (s *Server) apiRules(ctx context.Context) ([]store.Rule, error) {
	rules, err := s.st.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.Rule, 0, len(rules))
	for _, rule := range rules {
		if rule.APIEnabled {
			out = append(out, rule)
		}
	}
	return out, nil
}
