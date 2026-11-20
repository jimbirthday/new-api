package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestUpstreamBalanceProtocols(t *testing.T) {
	ctx := context.Background()
	sid := uuid.NewString()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/self":
			assert.Equal(t, "Bearer new-api-token", r.Header.Get("Authorization"))
			assert.Equal(t, "27", r.Header.Get("New-Api-User"))
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota":750000,"used_quota":9800000}}`))
		case "/api/status":
			assert.Empty(t, r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota_per_unit":500000}}`))
		case "/api/user/auth/refresh":
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, sid, r.Header.Get("X-Auth-Session"))
			assert.Equal(t, "https://"+r.Host, r.Header.Get("Origin"))
			cookie, err := r.Cookie(RefreshCookieName)
			if assert.NoError(t, err) {
				assert.Equal(t, sid+".old", cookie.Value)
			}
			http.SetCookie(w, &http.Cookie{Name: RefreshCookieName, Value: sid + ".rotated", HttpOnly: true, Secure: true})
			_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"new-api-token","access_expires_at":2000}}`))
		case "/api/v1/auth/refresh":
			assert.Equal(t, http.MethodPost, r.Method)
			var body map[string]string
			assert.NoError(t, common.DecodeJson(r.Body, &body))
			assert.Equal(t, "rt_previous", body["refresh_token"])
			_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"sub-token","refresh_token":"rt_next","expires_in":300}}`))
		case "/api/v1/auth/me":
			switch r.Header.Get("Authorization") {
			case "Bearer sub-token":
				_, _ = w.Write([]byte(`{"code":0,"data":{"balance":0}}`))
			case "Bearer missing":
				_, _ = w.Write([]byte(`{"code":0,"data":{"id":27}}`))
			default:
				w.WriteHeader(http.StatusUnauthorized)
			}
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		provider, refresh, rotated string
		userID                     int
		balance                    float64
		expiry                     int64
	}{
		{UpstreamBalanceNewAPI, sid + ".old", sid + ".rotated", 27, 1.5, 2000},
		{UpstreamBalanceSub2API, "rt_previous", "rt_next", 0, 0, 1300},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			m := &model.UpstreamBalanceMonitor{Provider: tc.provider, BaseURL: server.URL, UpstreamUserID: tc.userID}
			credential, expiry, err := refreshUpstreamAccount(ctx, server.Client(), m, UpstreamBalanceCredential{Refresh: tc.refresh}, 1000)
			require.NoError(t, err)
			assert.Equal(t, tc.rotated, credential.Refresh)
			assert.Equal(t, tc.expiry, expiry)
			balance, err := queryUpstreamAccountBalance(ctx, server.Client(), m, credential)
			require.NoError(t, err)
			assert.Equal(t, tc.balance, balance)
		})
	}
	m := &model.UpstreamBalanceMonitor{Provider: UpstreamBalanceSub2API, BaseURL: server.URL}
	_, err := queryUpstreamAccountBalance(ctx, server.Client(), m, UpstreamBalanceCredential{Access: "expired"})
	require.ErrorIs(t, err, errUpstreamCredential)
	_, err = queryUpstreamAccountBalance(ctx, server.Client(), m, UpstreamBalanceCredential{Access: "missing"})
	require.ErrorIs(t, err, errUpstreamRequest)
	for _, url := range []string{"http://example.com", "https://name@upstream.example", "https://upstream.example?a=b", "https://upstream.example/../outside"} {
		_, err := NormalizeUpstreamBalanceURL(url)
		assert.Error(t, err)
	}
	assert.ErrorIs(t, upstreamBalanceHTTP.CheckRedirect(nil, nil), http.ErrUseLastResponse)
	assert.False(t, (UpstreamBalanceCredential{Access: "token\r\nInjected: yes"}).Valid(UpstreamBalanceSub2API))
}

func TestUpstreamBalanceDatabaseMatrix(t *testing.T) {
	ctx := context.Background()
	previousSecret := common.CryptoSecret
	common.CryptoSecret = "test-only-persistent-credential-key"
	t.Setenv("CRYPTO_SECRET", common.CryptoSecret)
	t.Cleanup(func() { common.CryptoSecret = previousSecret })
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := upstreamMonitorTestDB(t, dialect)
			channel := model.Channel{Name: "balance integration", Key: "relay-secret", Status: common.ChannelStatusEnabled}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, db.AutoMigrate(&model.UpstreamBalanceMonitor{}, &model.UpstreamBalanceVoice{}))
			require.NoError(t, db.AutoMigrate(&model.UpstreamBalanceMonitor{}, &model.UpstreamBalanceVoice{}))
			legacy, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Equal(t, channel.Key, legacy.Key)

			now := common.GetTimestamp()
			amount := "4"
			reject := false
			rotations := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/auth/refresh" {
					rotations++
					var body map[string]string
					assert.NoError(t, common.DecodeJson(r.Body, &body))
					assert.Equal(t, "rt_old", body["refresh_token"])
					_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"new-token","refresh_token":"rt_new","expires_in":300}}`))
					return
				}
				assert.Equal(t, "/api/v1/auth/me", r.URL.Path)
				assert.Equal(t, "Bearer new-token", r.Header.Get("Authorization"))
				if reject {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_, _ = w.Write([]byte(`{"code":0,"data":{"balance":` + amount + `}}`))
			}))
			t.Cleanup(server.Close)

			m := model.UpstreamBalanceMonitor{
				ChannelID: channel.Id, Version: uuid.NewString(), Provider: UpstreamBalanceSub2API,
				BaseURL: server.URL, QueryMinutes: 15, RefreshMinutes: 5, AlertEnabled: true, ThresholdUSD: 5,
				Phone: "+8613800000000", NextQueryAt: now, NextRefreshAt: now,
			}
			initial := UpstreamBalanceCredential{Access: "expired-token", Refresh: "rt_old"}
			m.Credential, err = EncryptUpstreamBalanceSecret(initial, UpstreamBalanceSecretBinding(&m))
			require.NoError(t, err)
			m.SessionKey = UpstreamBalanceSessionKey(&m, initial)
			require.NoError(t, model.SaveUpstreamBalanceMonitor(ctx, &m, "", true))
			require.NotContains(t, m.Credential, initial.Refresh)
			var raw UpstreamBalanceCredential
			require.Error(t, decryptUpstreamBalanceSecret(m.Credential, UpstreamBalanceSecretBinding(&m)+"/other", &raw))

			another := model.Channel{Name: "another channel", Key: "other"}
			require.NoError(t, db.Create(&another).Error)
			duplicate := m
			duplicate.ChannelID = another.Id
			require.ErrorIs(t, model.SaveUpstreamBalanceMonitor(ctx, &duplicate, "", true), gorm.ErrDuplicatedKey)

			voice := model.UpstreamBalanceVoice{AppID: "123", TemplateID: "456"}
			voice.Credential, err = EncryptUpstreamBalanceSecret(UpstreamBalanceVoiceCredential{SecretID: "foo", SecretKey: "bar"}, UpstreamBalanceVoiceBinding)
			require.NoError(t, err)
			require.NoError(t, model.SaveUpstreamBalanceVoice(ctx, voice))

			previousTransport := upstreamBalanceVoiceTransport
			t.Cleanup(func() { upstreamBalanceVoiceTransport = previousTransport })
			calls := 0
			voiceTimeout := false
			upstreamBalanceVoiceTransport = upstreamBalanceTestTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				assert.Equal(t, "vms.tencentcloudapi.com", req.URL.Host)
				assert.Equal(t, "SendTtsVoice", req.Header.Get("X-TC-Action"))
				assert.Contains(t, req.Header.Get("Authorization"), "Credential=foo/")
				var voiceRequest struct {
					VoiceSdkAppid string
					TemplateId    string
					CalledNumber  string
				}
				assert.NoError(t, common.DecodeJson(req.Body, &voiceRequest))
				assert.Equal(t, "123", voiceRequest.VoiceSdkAppid)
				assert.Equal(t, "456", voiceRequest.TemplateId)
				assert.Equal(t, m.Phone, voiceRequest.CalledNumber)
				if voiceTimeout {
					return nil, context.DeadlineExceeded
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
					`{"Response":{"SendStatus":{"CallId":"test-call-id"},"RequestId":"test-request-id"}}`,
				))}, nil
			})

			require.NoError(t, runUpstreamBalanceMonitor(ctx, server.Client(), &m))
			stored, err := model.FindUpstreamBalanceMonitor(ctx, channel.Id)
			require.NoError(t, err)
			require.NotNil(t, stored.BalanceUSD)
			assert.Equal(t, 4.0, *stored.BalanceUSD)
			assert.Equal(t, 1, rotations)
			assert.Equal(t, "test-call-id", stored.AlertCallID)
			require.NoError(t, decryptUpstreamBalanceSecret(stored.Credential, UpstreamBalanceSecretBinding(stored), &raw))
			assert.Equal(t, UpstreamBalanceCredential{Access: "new-token", Refresh: "rt_new"}, raw)
			metadata, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.True(t, metadata.GetBalanceDetail().UpstreamAccount)
			assert.Equal(t, 4.0, metadata.Balance)
			assert.Equal(t, common.ChannelStatusEnabled, metadata.Status)

			for _, step := range []struct {
				balance   string
				reject    bool
				timeout   bool
				wantCalls int
				wantState string
			}{
				{balance: "4", wantCalls: 1, wantState: "submitted"},
				{balance: "0", reject: true, wantCalls: 1, wantState: "submitted"},
				{balance: "6", wantCalls: 1},
				{balance: "5", timeout: true, wantCalls: 2, wantState: "unknown"},
				{balance: "5", wantCalls: 2, wantState: "unknown"},
				{balance: "6", wantCalls: 2},
				{balance: "5", wantCalls: 3, wantState: "submitted"},
			} {
				amount, reject, voiceTimeout = step.balance, step.reject, step.timeout
				queued, err := model.RequestUpstreamBalance(ctx, channel.Id)
				require.NoError(t, err)
				require.True(t, queued)
				due, err := model.FindDueUpstreamBalanceMonitors(ctx, now)
				require.NoError(t, err)
				require.Len(t, due, 1)
				require.NoError(t, runUpstreamBalanceMonitor(ctx, server.Client(), &due[0]))
				stored, err := model.FindUpstreamBalanceMonitor(ctx, channel.Id)
				require.NoError(t, err)
				assert.Equal(t, step.wantCalls, calls)
				assert.Equal(t, step.wantState, stored.AlertState)
				assert.Equal(t, stored.RequestedAt, stored.QueriedRequest)
				if reject {
					assert.Equal(t, "credentials", stored.QueryError)
					assert.Equal(t, 4.0, *stored.BalanceUSD)
				}
			}
			require.NoError(t, model.RemoveUpstreamBalanceMonitor(ctx, channel.Id))
			metadata, err = model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Nil(t, metadata.GetBalanceDetail())
			assert.Equal(t, channel.Key, metadata.Key)
		})
	}
}

type upstreamBalanceTestTransport func(*http.Request) (*http.Response, error)

func (transport upstreamBalanceTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func upstreamMonitorTestDB(t *testing.T, dialect string) *gorm.DB {
	t.Helper()
	oldDB, oldType := model.DB, common.MainDatabaseType()
	var driver gorm.Dialector
	var dbType common.DatabaseType
	var dsn string
	switch dialect {
	case "sqlite":
		driver, dbType = sqlite.Open(filepath.Join(t.TempDir(), "main.db")), common.DatabaseTypeSQLite
	case "mysql":
		dsn = os.Getenv("TEST_MYSQL_DSN")
		if dsn == "" {
			t.Skip("TEST_MYSQL_DSN is not configured")
		}
		driver, dbType = mysql.Open(dsn), common.DatabaseTypeMySQL
	case "postgres":
		dsn = os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not configured")
		}
		driver, dbType = postgres.Open(dsn), common.DatabaseTypePostgreSQL
	}
	db, err := gorm.Open(driver, &gorm.Config{TranslateError: true})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	model.DB = db
	common.SetMainDatabaseType(dbType)
	t.Cleanup(func() {
		model.DB = oldDB
		common.SetMainDatabaseType(oldType)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.Channel{}))
	var version string
	versionSQL := "SELECT VERSION()"
	if dialect == "sqlite" {
		versionSQL = "SELECT sqlite_version()"
	}
	require.NoError(t, db.Raw(versionSQL).Scan(&version).Error)
	t.Logf("database version: %s", version)
	return db
}
