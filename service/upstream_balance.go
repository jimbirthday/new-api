package service

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/golang-jwt/jwt/v5"
)

const (
	UpstreamBalanceNewAPI  = "new-api"
	UpstreamBalanceSub2API = "sub2api"

	upstreamCredentialError = "credentials"
	upstreamRequestError    = "upstream"
	upstreamStorageError    = "storage"
)

var (
	errUpstreamCredential = errors.New(upstreamCredentialError)
	errUpstreamRequest    = errors.New(upstreamRequestError)
	errUpstreamStorage    = errors.New(upstreamStorageError)
)

type UpstreamBalanceCredential struct {
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
}

func UpstreamBalanceEncryptionConfigured() bool {
	secret := os.Getenv("CRYPTO_SECRET")
	if secret == "" {
		secret = os.Getenv("SESSION_SECRET")
	}
	return len(strings.TrimSpace(secret)) >= 32
}

func EncryptUpstreamBalanceSecret(value any, binding string) (string, error) {
	aead, err := upstreamBalanceAEAD()
	if err != nil {
		return "", err
	}
	data, err := common.Marshal(value)
	if err != nil {
		return "", errUpstreamStorage
	}
	return base64.RawStdEncoding.EncodeToString(aead.Seal(nil, nil, data, []byte(binding))), nil
}

func decryptUpstreamBalanceSecret(value, binding string, dest any) error {
	aead, err := upstreamBalanceAEAD()
	if err != nil {
		return err
	}
	encrypted, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		return errUpstreamStorage
	}
	clear, err := aead.Open(nil, nil, encrypted, []byte(binding))
	if err != nil || common.Unmarshal(clear, dest) != nil {
		return errUpstreamStorage
	}
	return nil
}

func upstreamBalanceAEAD() (cipher.AEAD, error) {
	if !UpstreamBalanceEncryptionConfigured() {
		return nil, errUpstreamStorage
	}
	key, err := hkdf.Key(sha256.New, []byte(common.CryptoSecret), nil, "upstream-balance-credentials", 32)
	if err != nil {
		return nil, errUpstreamStorage
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errUpstreamStorage
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func NormalizeUpstreamBalanceURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || len(raw) > 1024 {
		return "", errUpstreamRequest
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.ContainsAny(u.Path, "\\%") || strings.Contains(u.Path, "//") {
		return "", errUpstreamRequest
	}
	for part := range strings.SplitSeq(u.Path, "/") {
		if part == "." || part == ".." {
			return "", errUpstreamRequest
		}
	}
	return u.String(), nil
}

func (credential UpstreamBalanceCredential) Valid(provider string) bool {
	if credential.Access == "" && credential.Refresh == "" {
		return false
	}
	for _, token := range []string{credential.Access, credential.Refresh} {
		if len(token) > 8192 || strings.ContainsFunc(token, func(r rune) bool { return r < 33 || r > 126 }) {
			return false
		}
	}
	if provider == UpstreamBalanceNewAPI && credential.Refresh != "" {
		cookie := http.Cookie{Name: RefreshCookieName, Value: credential.Refresh}
		sid, ok := RefreshTokenSID(credential.Refresh)
		accessSID := upstreamBalanceJWTClaims(credential.Access).SessionID
		return ok && cookie.Valid() == nil && (accessSID == "" || accessSID == sid)
	}
	return true
}

func (credential UpstreamBalanceCredential) TokenExpiry() int64 {
	claims := upstreamBalanceJWTClaims(credential.Access)
	if claims.ExpiresAt == nil {
		return 0
	}
	return claims.ExpiresAt.Unix()
}

type upstreamBalanceClaims struct {
	SessionID string `json:"sid"`
	UserID    int64  `json:"user_id"`
	jwt.RegisteredClaims
}

func upstreamBalanceJWTClaims(raw string) upstreamBalanceClaims {
	var claims upstreamBalanceClaims
	if _, _, err := jwt.NewParser().ParseUnverified(raw, &claims); err != nil {
		return upstreamBalanceClaims{}
	}
	return claims
}

func UpstreamBalanceSessionKey(m *model.UpstreamBalanceMonitor, credential UpstreamBalanceCredential) *string {
	if credential.Refresh == "" {
		return nil
	}
	sid := upstreamBalanceJWTClaims(credential.Access).SessionID
	if m.Provider == UpstreamBalanceNewAPI {
		sid, _ = RefreshTokenSID(credential.Refresh)
	}
	kind := "session"
	if sid == "" {
		kind = "refresh"
		sid = credential.Refresh
	}
	// JWT claims are used only to coordinate refresh, and never authorize access.
	hash := hmac.New(sha256.New, []byte(common.CryptoSecret))
	site, _ := url.Parse(m.BaseURL)
	hash.Write([]byte(m.Provider + "\x00" + site.Hostname() + "\x00" + kind + "\x00" + sid))
	digest := hex.EncodeToString(hash.Sum(nil))
	return &digest
}

func UpstreamBalanceSecretBinding(m *model.UpstreamBalanceMonitor) string {
	return "channel/" + strconv.Itoa(m.ChannelID) + "/" + m.Provider + "/" + m.BaseURL + "/" + strconv.Itoa(m.UpstreamUserID)
}

var upstreamBalanceHTTP = &http.Client{
	Timeout:       12 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		ForceAttemptHTTP2: true,
		IdleConnTimeout:   90 * time.Second,
		DialContext: (&protectedFetchDialer{
			resolver:    net.DefaultResolver,
			dialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			getProtection: func() (*common.SSRFProtection, bool, error) {
				protection, enabled, err := currentFetchProtection()
				if err != nil {
					return nil, true, err
				}
				if !enabled {
					defaults := *common.DefaultSSRFProtection
					protection = &defaults
				}
				protection.ApplyIPFilterForDomain = true
				return protection, true, nil
			},
		}).DialContext,
	},
}

type upstreamBalanceResponse struct {
	Success *bool `json:"success"`
	Code    *int  `json:"code"`
	Data    struct {
		Quota           *int64   `json:"quota"`
		QuotaPerUnit    float64  `json:"quota_per_unit"`
		Balance         *float64 `json:"balance"`
		AccessToken     string   `json:"access_token"`
		RefreshToken    string   `json:"refresh_token"`
		AccessExpiresAt int64    `json:"access_expires_at"`
		ExpiresIn       int64    `json:"expires_in"`
	} `json:"data"`
}

func callUpstreamBalance(ctx context.Context, client *http.Client, m *model.UpstreamBalanceMonitor, path string, credential UpstreamBalanceCredential) (upstreamBalanceResponse, []*http.Cookie, error) {
	var result upstreamBalanceResponse
	method := http.MethodGet
	var body []byte
	isRefresh := credential.Refresh != ""
	if isRefresh {
		method = http.MethodPost
		if m.Provider == UpstreamBalanceSub2API {
			body, _ = common.Marshal(map[string]string{"refresh_token": credential.Refresh})
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, m.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return result, nil, errUpstreamRequest
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if isRefresh && m.Provider == UpstreamBalanceNewAPI {
		req.AddCookie(&http.Cookie{Name: RefreshCookieName, Value: credential.Refresh})
		if sid, ok := RefreshTokenSID(credential.Refresh); ok {
			req.Header.Set("X-Auth-Session", sid)
		}
		req.Header.Set("Origin", req.URL.Scheme+"://"+req.URL.Host)
	}
	if credential.Access != "" {
		req.Header.Set("Authorization", "Bearer "+credential.Access)
		if m.Provider == UpstreamBalanceNewAPI && m.UpstreamUserID > 0 {
			req.Header.Set("New-Api-User", strconv.Itoa(m.UpstreamUserID))
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return result, nil, errUpstreamRequest
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		(isRefresh && resp.StatusCode == http.StatusBadRequest) {
		return result, nil, errUpstreamCredential
	}
	if resp.StatusCode != http.StatusOK {
		return result, nil, errUpstreamRequest
	}
	const maxBody = 512 << 10
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(data) > maxBody {
		return result, nil, errUpstreamRequest
	}
	if common.Unmarshal(data, &result) != nil {
		return result, nil, errUpstreamRequest
	}
	success := result.Success != nil && *result.Success
	if m.Provider == UpstreamBalanceSub2API {
		success = result.Code != nil && *result.Code == 0
	}
	if !success {
		if isRefresh || credential.Access != "" {
			return result, nil, errUpstreamCredential
		}
		return result, nil, errUpstreamRequest
	}
	return result, resp.Cookies(), nil
}

func queryUpstreamAccountBalance(ctx context.Context, client *http.Client, m *model.UpstreamBalanceMonitor, credential UpstreamBalanceCredential) (float64, error) {
	if credential.Access == "" {
		return 0, errUpstreamCredential
	}
	auth := UpstreamBalanceCredential{Access: credential.Access}
	switch m.Provider {
	case UpstreamBalanceNewAPI:
		profile, _, err := callUpstreamBalance(ctx, client, m, "/api/user/self", auth)
		if err != nil {
			return 0, err
		}
		status, _, err := callUpstreamBalance(ctx, client, m, "/api/status", UpstreamBalanceCredential{})
		if err != nil || profile.Data.Quota == nil || status.Data.QuotaPerUnit <= 0 {
			return 0, errUpstreamRequest
		}
		balance := float64(*profile.Data.Quota) / status.Data.QuotaPerUnit
		if math.IsInf(balance, 0) {
			return 0, errUpstreamRequest
		}
		return balance, nil
	case UpstreamBalanceSub2API:
		profile, _, err := callUpstreamBalance(ctx, client, m, "/api/v1/auth/me", auth)
		if err != nil {
			return 0, err
		}
		if profile.Data.Balance == nil {
			return 0, errUpstreamRequest
		}
		return *profile.Data.Balance, nil
	default:
		return 0, errUpstreamRequest
	}
}

func refreshUpstreamAccount(ctx context.Context, client *http.Client, m *model.UpstreamBalanceMonitor, credential UpstreamBalanceCredential, now int64) (UpstreamBalanceCredential, int64, error) {
	path := "/api/v1/auth/refresh"
	if m.Provider == UpstreamBalanceNewAPI {
		path = "/api/user/auth/refresh"
	}
	response, cookies, err := callUpstreamBalance(ctx, client, m, path, UpstreamBalanceCredential{Refresh: credential.Refresh})
	if err != nil {
		return UpstreamBalanceCredential{}, 0, err
	}
	next := UpstreamBalanceCredential{Access: response.Data.AccessToken, Refresh: response.Data.RefreshToken}
	expiry := int64(0)
	if response.Data.ExpiresIn > 0 && response.Data.ExpiresIn <= 366*86400 {
		expiry = now + response.Data.ExpiresIn
	}
	if m.Provider == UpstreamBalanceNewAPI {
		expiry = response.Data.AccessExpiresAt
		for _, cookie := range cookies {
			if cookie.Name == RefreshCookieName && cookie.MaxAge >= 0 {
				next.Refresh = cookie.Value
				break
			}
		}
	}
	if next.Access == "" || next.Refresh == "" || !next.Valid(m.Provider) {
		return UpstreamBalanceCredential{}, 0, errUpstreamRequest
	}
	before := upstreamBalanceJWTClaims(credential.Access)
	after := upstreamBalanceJWTClaims(next.Access)
	if before.SessionID != "" && after.SessionID != before.SessionID ||
		before.UserID > 0 && after.UserID != before.UserID {
		return UpstreamBalanceCredential{}, 0, errUpstreamCredential
	}
	if expiry <= now {
		expiry = next.TokenExpiry()
	}
	return next, expiry, nil
}
