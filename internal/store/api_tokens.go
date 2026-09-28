package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// apiTokensSettingKey holds the JSON token list. Only the SHA-256 of a token is
// persisted, so a leaked database cannot be replayed against the API.
const apiTokensSettingKey = "api_tokens"

// apiTokenPrefix makes a leaked token recognisable in logs and scanners.
const apiTokenPrefix = "rst_"

// lastUsedThrottle avoids one write per API request on a single-connection DB.
const lastUsedThrottle = time.Minute

var ErrAPITokenNotFound = errors.New("api token not found")

// APIToken is one credential for /api/v1. Times are unix seconds to match the
// rest of the store; 0 means "never".
type APIToken struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hash       string `json:"hash"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
	LastUsedAt int64  `json:"last_used_at"`
}

// Active reports whether the token may authenticate a request right now.
func (t APIToken) Active(now time.Time) bool {
	if !t.Enabled {
		return false
	}
	if t.ExpiresAt > 0 && now.Unix() >= t.ExpiresAt {
		return false
	}
	return true
}

func (s *Store) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	raw, ok, err := s.Setting(ctx, apiTokensSettingKey)
	if err != nil {
		return nil, err
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var tokens []APIToken
	if err := json.Unmarshal([]byte(raw), &tokens); err != nil {
		return nil, err
	}
	return tokens, nil
}

func (s *Store) editAPITokens(ctx context.Context, edit func(*[]APIToken) (bool, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, apiTokensSettingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	tokens := []APIToken{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &tokens); err != nil {
			return err
		}
	}
	changed, err := edit(&tokens)
	if err != nil {
		return err
	}
	if changed {
		encoded, err := json.Marshal(tokens)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, apiTokensSettingKey, string(encoded), nowUnix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) CreateAPIToken(ctx context.Context, name string, expiresAt time.Time) (APIToken, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return APIToken{}, "", errors.New("token name required")
	}
	plaintext, err := newAPITokenSecret()
	if err != nil {
		return APIToken{}, "", err
	}
	token := APIToken{ID: newTokenID(), Name: name, Hash: HashAPIToken(plaintext), Enabled: true, CreatedAt: nowUnix()}
	if !expiresAt.IsZero() {
		token.ExpiresAt = expiresAt.Unix()
	}
	err = s.editAPITokens(ctx, func(tokens *[]APIToken) (bool, error) { *tokens = append(*tokens, token); return true, nil })
	return token, plaintext, err
}
func (s *Store) SetAPITokenEnabled(ctx context.Context, id string, enabled bool) error {
	return s.editAPITokens(ctx, func(tokens *[]APIToken) (bool, error) {
		for i := range *tokens {
			if (*tokens)[i].ID == id {
				(*tokens)[i].Enabled = enabled
				return true, nil
			}
		}
		return false, ErrAPITokenNotFound
	})
}
func (s *Store) DeleteAPIToken(ctx context.Context, id string) error {
	return s.editAPITokens(ctx, func(tokens *[]APIToken) (bool, error) {
		for i := range *tokens {
			if (*tokens)[i].ID == id {
				*tokens = append((*tokens)[:i], (*tokens)[i+1:]...)
				return true, nil
			}
		}
		return false, ErrAPITokenNotFound
	})
}
func (s *Store) AuthenticateAPIToken(ctx context.Context, plaintext string) (APIToken, bool, error) {
	plaintext = strings.TrimSpace(plaintext)
	if plaintext == "" {
		return APIToken{}, false, nil
	}
	var result APIToken
	found := false
	err := s.editAPITokens(ctx, func(tokens *[]APIToken) (bool, error) {
		now := time.Now()
		wanted := []byte(HashAPIToken(plaintext))
		matched := -1
		for i, t := range *tokens {
			if subtle.ConstantTimeCompare([]byte(t.Hash), wanted) == 1 && t.Active(now) {
				matched = i
			}
		}
		if matched < 0 {
			return false, nil
		}
		result = (*tokens)[matched]
		found = true
		if now.Unix()-result.LastUsedAt < int64(lastUsedThrottle.Seconds()) {
			return false, nil
		}
		(*tokens)[matched].LastUsedAt = now.Unix()
		result = (*tokens)[matched]
		return true, nil
	})
	return result, found, err
}

// HashAPIToken is the single hashing definition shared by minting and auth.
func HashAPIToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func newAPITokenSecret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return apiTokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// NewCallbackSecret mints a shared HMAC secret. It lives next to token minting
// so the page and the CLI cannot drift apart on entropy or encoding.
func NewCallbackSecret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func newTokenID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}
