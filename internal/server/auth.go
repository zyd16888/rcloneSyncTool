package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

const (
	authCookieName     = "rclone_syncd_auth"
	authCookieMaxAge   = 30 * 24 * time.Hour
	authSecretKey      = "ui_auth_secret"
	authPasswordHashKey = "ui_password_hash"
)

type uiAuthConfig struct {
	PasswordHash string
	Secret       []byte
	HasPassword  bool
}

func (s *Server) uiAuthConfig(ctx *gin.Context) (uiAuthConfig, error) {
	secretB64, ok, err := s.st.Setting(ctx.Request.Context(), authSecretKey)
	if err != nil {
		return uiAuthConfig{}, err
	}
	if !ok || strings.TrimSpace(secretB64) == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return uiAuthConfig{}, err
		}
		secretB64 = base64.StdEncoding.EncodeToString(raw)
		if err := s.st.SetSetting(ctx.Request.Context(), authSecretKey, secretB64); err != nil {
			return uiAuthConfig{}, err
		}
	}
	secret, err := base64.StdEncoding.DecodeString(secretB64)
	if err != nil || len(secret) < 16 {
		return uiAuthConfig{}, errors.New("invalid ui_auth_secret")
	}

	pwdHash, ok, err := s.st.Setting(ctx.Request.Context(), authPasswordHashKey)
	if err != nil {
		return uiAuthConfig{}, err
	}
	pwdHash = strings.TrimSpace(pwdHash)
	return uiAuthConfig{
		PasswordHash: pwdHash,
		Secret:       secret,
		HasPassword:  ok && pwdHash != "",
	}, nil
}

func signHMAC(secret []byte, msg string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func issueAuthCookie(c *gin.Context, cfg uiAuthConfig) error {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonceB64 := base64.RawURLEncoding.EncodeToString(nonce)
	msg := ts + "." + nonceB64 + "." + cfg.PasswordHash
	sig := signHMAC(cfg.Secret, msg)
	val := "v1." + ts + "." + nonceB64 + "." + sig

	c.SetCookie(authCookieName, val, int(authCookieMaxAge.Seconds()), "/", "", false, true)
	return nil
}

func clearAuthCookie(c *gin.Context) {
	c.SetCookie(authCookieName, "", -1, "/", "", false, true)
}

func isAuthed(c *gin.Context, cfg uiAuthConfig) bool {
	if !cfg.HasPassword {
		return false
	}
	val, err := c.Cookie(authCookieName)
	if err != nil {
		return false
	}
	parts := strings.Split(val, ".")
	if len(parts) != 4 {
		return false
	}
	if parts[0] != "v1" {
		return false
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	nonceB64 := parts[2]
	sig := parts[3]
	if nonceB64 == "" || sig == "" {
		return false
	}

	now := time.Now()
	t := time.Unix(ts, 0)
	if t.After(now.Add(2*time.Minute)) || now.Sub(t) > authCookieMaxAge {
		return false
	}

	msg := parts[1] + "." + nonceB64 + "." + cfg.PasswordHash
	expected := signHMAC(cfg.Secret, msg)
	return hmac.Equal([]byte(expected), []byte(sig))
}

func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/static/") || p == "/login" || p == "/logout" {
			c.Next()
			return
		}

		cfg, err := s.uiAuthConfig(c)
		if err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		if !cfg.HasPassword {
			if strings.HasPrefix(p, "/api/") {
				c.JSON(http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}
			c.Redirect(http.StatusSeeOther, "/login?next="+urlQueryEscape(c.Request.URL.RequestURI()))
			return
		}
		if isAuthed(c, cfg) {
			c.Next()
			return
		}
		if strings.HasPrefix(p, "/api/") {
			c.JSON(http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		c.Redirect(http.StatusSeeOther, "/login?next="+urlQueryEscape(c.Request.URL.RequestURI()))
	}
}

func urlQueryEscape(s string) string {
	// keep it local and simple (avoid importing net/url in hot path)
	r := strings.NewReplacer(
		"%", "%25",
		" ", "%20",
		"?", "%3F",
		"&", "%26",
		"#", "%23",
		"=", "%3D",
		"+", "%2B",
	)
	return r.Replace(s)
}

func (s *Server) loginGet(c *gin.Context) {
	cfg, err := s.uiAuthConfig(c)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	if cfg.HasPassword && isAuthed(c, cfg) {
		next := strings.TrimSpace(c.Query("next"))
		if next == "" || !strings.HasPrefix(next, "/") {
			next = "/"
		}
		s.redirect(c, next)
		return
	}
	s.render(c, "login", map[string]any{
		"Active":      "",
		"HasPassword": cfg.HasPassword,
		"Next":        c.Query("next"),
	})
}

func (s *Server) loginPost(c *gin.Context) {
	cfg, err := s.uiAuthConfig(c)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	next := strings.TrimSpace(c.PostForm("next"))
	if next == "" || !strings.HasPrefix(next, "/") {
		next = "/"
	}

	if !cfg.HasPassword {
		p1 := c.PostForm("password")
		p2 := c.PostForm("password2")
		if strings.TrimSpace(p1) == "" {
			s.render(c, "login", map[string]any{
				"Active":      "",
				"HasPassword": false,
				"Error":       "请输入新密码",
				"Next":        next,
			})
			return
		}
		if p1 != p2 {
			s.render(c, "login", map[string]any{
				"Active":      "",
				"HasPassword": false,
				"Error":       "两次输入的密码不一致",
				"Next":        next,
			})
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(p1), bcrypt.DefaultCost)
		if err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		if err := s.st.SetSetting(c.Request.Context(), authPasswordHashKey, string(hash)); err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		cfg.PasswordHash = string(hash)
		cfg.HasPassword = true
		if err := issueAuthCookie(c, cfg); err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		s.redirect(c, next)
		return
	}

	p := c.PostForm("password")
	if bcrypt.CompareHashAndPassword([]byte(cfg.PasswordHash), []byte(p)) != nil {
		clearAuthCookie(c)
		s.render(c, "login", map[string]any{
			"Active":      "",
			"HasPassword": true,
			"Error":       "密码错误",
			"Next":        next,
		})
		return
	}
	if err := issueAuthCookie(c, cfg); err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	s.redirect(c, next)
}

func (s *Server) logoutPost(c *gin.Context) {
	clearAuthCookie(c)
	s.redirect(c, "/login")
}
