package daemon

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func driveAccessToken(ctx context.Context, config map[string]string) (string, error) {
	credentials := config["service_account_credentials"]
	if p := config["service_account_file"]; p != "" {
		data, err := os.ReadFile(os.ExpandEnv(p))
		if err != nil {
			return "", errors.New("无法读取 Drive service account 文件")
		}
		credentials = string(data)
	}
	if credentials == "" {
		var token struct {
			AccessToken string `json:"access_token"`
		}
		if json.Unmarshal([]byte(config["token"]), &token) != nil || token.AccessToken == "" {
			return "", errors.New("无法获取 Drive 发布凭据")
		}
		return token.AccessToken, nil
	}
	var account struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if json.Unmarshal([]byte(credentials), &account) != nil || account.ClientEmail == "" {
		return "", errors.New("Drive service account 配置无效")
	}
	block, _ := pem.Decode([]byte(account.PrivateKey))
	if block == nil {
		return "", errors.New("Drive service account 私钥无效")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", errors.New("无法解析 Drive service account 私钥")
	}
	privateKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return "", errors.New("Drive service account 需要 RSA 私钥")
	}
	if account.TokenURI == "" {
		account.TokenURI = "https://oauth2.googleapis.com/token"
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims := map[string]any{"iss": account.ClientEmail, "scope": "https://www.googleapis.com/auth/drive", "aud": account.TokenURI, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	if subject := config["impersonate"]; subject != "" {
		claims["sub"] = subject
	}
	body, _ := json.Marshal(claims)
	assertion := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(assertion))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("Drive 授权签名失败")
	}
	assertion += "." + base64.RawURLEncoding.EncodeToString(signature)
	values := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, account.TokenURI, strings.NewReader(values.Encode()))
	if err != nil {
		return "", errors.New("Drive token URI 无效")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("Drive service account 授权失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("Drive service account 授权被拒绝")
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token) != nil || token.AccessToken == "" {
		return "", errors.New("Drive 授权响应无效")
	}
	return token.AccessToken, nil
}
