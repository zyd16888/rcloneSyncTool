package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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

func (s *Store) saveAPITokens(ctx context.Context, tokens []APIToken) error {
	if tokens == nil {
		tokens = []APIToken{}
	}
	b, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, apiTokensSettingKey, string(b))
}

// CreateAPIToken mints a token and returns the plaintext exactly once. The
// caller must show it to the operator immediately; it cannot be recovered.
func (s *Store) CreateAPIToken(ctx context.Context, name string, expiresAt time.Time) (APIToken, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return APIToken{}, "", errors.New("token name required")
	}
	tokens, err := s.ListAPITokens(ctx)
	if err != nil {
		return APIToken{}, "", err
	}
	plaintext, err := newAPITokenSecret()
	if err != nil {
		return APIToken{}, "", err
	}
	token := APIToken{
		ID:        newTokenID(),
		Name:      name,
		Hash:      HashAPIToken(plaintext),
		Enabled:   true,
		CreatedAt: nowUnix(),
	}
	if !expiresAt.IsZero() {
		token.ExpiresAt = expiresAt.Unix()
	}
	tokens = append(tokens, token)
	if err := s.saveAPITokens(ctx, tokens); err != nil {
		return APIToken{}, "", err
	}
	return token, plaintext, nil
}

func (s *Store) SetAPITokenEnabled(ctx context.Context, id string, enabled bool) error {
	tokens, err := s.ListAPITokens(ctx)
	if err != nil {
		return err
	}
	found := false
	for i := range tokens {
		if tokens[i].ID == id {
			tokens[i].Enabled = enabled
			found = true
			break
		}
	}
	if !found {
		return ErrAPITokenNotFound
	}
	return s.saveAPITokens(ctx, tokens)
}

func (s *Store) DeleteAPIToken(ctx context.Context, id string) error {
	tokens, err := s.ListAPITokens(ctx)
	if err != nil {
		return err
	}
	kept := make([]APIToken, 0, len(tokens))
	for _, token := range tokens {
		if token.ID != id {
			kept = append(kept, token)
		}
	}
	if len(kept) == len(tokens) {
		return ErrAPITokenNotFound
	}
	return s.saveAPITokens(ctx, kept)
}

// AuthenticateAPIToken resolves a plaintext bearer token. Every candidate is
// compared in constant time and the loop never exits early, so a caller cannot
// learn which token it nearly matched from response timing.
func (s *Store) AuthenticateAPIToken(ctx context.Context, plaintext string) (APIToken, bool, error) {
	plaintext = strings.TrimSpace(plaintext)
	if plaintext == "" {
		return APIToken{}, false, nil
	}
	tokens, err := s.ListAPITokens(ctx)
	if err != nil {
		return APIToken{}, false, err
	}
	now := time.Now()
	wanted := []byte(HashAPIToken(plaintext))
	matched := -1
	for i, token := range tokens {
		hit := subtle.ConstantTimeCompare([]byte(token.Hash), wanted) == 1
		if hit && token.Active(now) {
			matched = i
		}
	}
	if matched < 0 {
		return APIToken{}, false, nil
	}
	token := tokens[matched]
	if now.Unix()-token.LastUsedAt >= int64(lastUsedThrottle.Seconds()) {
		tokens[matched].LastUsedAt = now.Unix()
		if err := s.saveAPITokens(ctx, tokens); err != nil {
			// A failed bookkeeping write must not reject a valid credential.
			return token, true, nil
		}
		token = tokens[matched]
	}
	return token, true, nil
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

func newTokenID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}
