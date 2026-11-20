package controller

import (
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var (
	upstreamBalancePhone   = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	upstreamBalanceVoiceID = regexp.MustCompile(`^[0-9]{1,32}$`)
)

func GetUpstreamBalanceMonitor(c *gin.Context) {
	channelID, err := strconv.Atoi(c.Param("id"))
	if err != nil || channelID <= 0 {
		common.ApiErrorT(c, "Invalid channel ID")
		return
	}
	monitor, err := model.FindUpstreamBalanceMonitor(c.Request.Context(), channelID)
	if err != nil {
		common.ApiErrorT(c, "Unable to load upstream balance settings")
		return
	}
	if monitor == nil {
		common.ApiSuccess(c, nil)
		return
	}
	common.ApiSuccess(c, struct {
		*model.UpstreamBalanceMonitor
		QueryPending bool `json:"query_pending"`
	}{monitor, monitor.RequestedAt > monitor.QueriedRequest})
}

func SaveUpstreamBalanceMonitor(c *gin.Context) {
	channelID, err := strconv.Atoi(c.Param("id"))
	if err != nil || channelID <= 0 {
		common.ApiErrorT(c, "Invalid channel ID")
		return
	}
	var request struct {
		model.UpstreamBalanceMonitor
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiErrorT(c, "Invalid upstream balance settings")
		return
	}
	item := &request.UpstreamBalanceMonitor
	item.ChannelID = channelID
	item.Version = uuid.NewString()
	item.Phone = strings.TrimSpace(item.Phone)
	if item.Provider != service.UpstreamBalanceNewAPI && item.Provider != service.UpstreamBalanceSub2API ||
		item.QueryMinutes < 0 || item.QueryMinutes > 1440 || item.RefreshMinutes < 0 || item.RefreshMinutes > 1440 ||
		item.UpstreamUserID < 0 || item.ThresholdUSD < 0 || math.IsInf(item.ThresholdUSD, 0) || math.IsNaN(item.ThresholdUSD) ||
		(item.AlertEnabled || item.Phone != "") && !upstreamBalancePhone.MatchString(item.Phone) {
		common.ApiErrorT(c, "Invalid upstream balance settings")
		return
	}
	if item.Provider == service.UpstreamBalanceSub2API {
		item.UpstreamUserID = 0
	}
	item.BaseURL, err = service.NormalizeUpstreamBalanceURL(item.BaseURL)
	if err != nil {
		common.ApiErrorT(c, "Enter a valid HTTPS upstream URL")
		return
	}
	if !service.UpstreamBalanceEncryptionConfigured() {
		common.ApiErrorT(c, "Set a persistent CRYPTO_SECRET or SESSION_SECRET of at least 32 characters")
		return
	}
	ctx := c.Request.Context()
	old, err := model.FindUpstreamBalanceMonitor(ctx, channelID)
	if err != nil {
		common.ApiErrorT(c, "Unable to load upstream balance settings")
		return
	}
	credential := service.UpstreamBalanceCredential{Access: strings.TrimSpace(request.Access), Refresh: strings.TrimSpace(request.Refresh)}
	replace := credential.Access != "" || credential.Refresh != ""
	if !replace && (old == nil || old.Provider != item.Provider || old.BaseURL != item.BaseURL || old.UpstreamUserID != item.UpstreamUserID) {
		common.ApiErrorT(c, "Enter new credentials when changing the upstream account or URL")
		return
	}
	// Runtime state is server-owned, including for newly created monitors.
	item.BalanceUSD, item.CheckedAt, item.AccessExpiresAt = nil, 0, 0
	item.AlertState, item.AlertCallID, item.QueryError, item.RefreshError = "", "", "", ""
	if replace {
		if !credential.Valid(item.Provider) || credential.Access == "" && item.RefreshMinutes == 0 ||
			item.RefreshMinutes > 0 && credential.Refresh == "" {
			common.ApiErrorT(c, "Enter valid upstream credentials and a refresh token for automatic renewal")
			return
		}
		item.Credential, err = service.EncryptUpstreamBalanceSecret(credential, service.UpstreamBalanceSecretBinding(item))
		if err != nil {
			common.ApiErrorT(c, "Unable to save upstream balance settings")
			return
		}
		item.SessionKey = service.UpstreamBalanceSessionKey(item, credential)
		item.AccessExpiresAt = credential.TokenExpiry()
	} else if item.RefreshMinutes > 0 && old.SessionKey == nil {
		common.ApiErrorT(c, "Enter valid upstream credentials and a refresh token for automatic renewal")
		return
	}
	var oldVersion string
	if old != nil {
		oldVersion = old.Version
	}
	model.QueueUpstreamBalanceOnSave(item)
	if !replace && old.NextRefreshAt == 0 && old.RefreshError != "" {
		item.NextRefreshAt = 0
	}
	if err := model.SaveUpstreamBalanceMonitor(ctx, item, oldVersion, replace); err != nil {
		if item.SessionKey != nil && model.UpstreamBalanceSessionInUse(ctx, *item.SessionKey, channelID) {
			common.ApiErrorT(c, "This upstream session is already configured on another channel")
			return
		}
		common.ApiErrorT(c, "Unable to save upstream balance settings")
		return
	}
	recordManageAudit(c, "channel.upstream_balance.update", map[string]any{"channel_id": channelID, "provider": item.Provider})
	common.ApiSuccess(c, nil)
}

func DeleteUpstreamBalanceMonitor(c *gin.Context) {
	channelID, err := strconv.Atoi(c.Param("id"))
	if err != nil || channelID <= 0 {
		common.ApiErrorT(c, "Invalid channel ID")
		return
	}
	if err := model.RemoveUpstreamBalanceMonitor(c.Request.Context(), channelID); err != nil {
		common.ApiErrorT(c, "Unable to save upstream balance settings")
		return
	}
	recordManageAudit(c, "channel.upstream_balance.remove", map[string]any{"channel_id": channelID})
	common.ApiSuccess(c, nil)
}

func QueueUpstreamBalanceQuery(c *gin.Context) {
	channelID, err := strconv.Atoi(c.Param("id"))
	if err != nil || channelID <= 0 {
		common.ApiErrorT(c, "Invalid channel ID")
		return
	}
	queued, err := model.RequestUpstreamBalance(c.Request.Context(), channelID)
	if err != nil || !queued {
		common.ApiErrorT(c, "Unable to queue upstream balance query")
		return
	}
	// The recurring job also picks up pending requests if a run is already active.
	_, _, _ = service.EnqueueSystemTask(model.SystemTaskTypeUpstreamBalance, nil)
	common.ApiSuccess(c, nil)
}

func GetUpstreamBalanceVoice(c *gin.Context) {
	config, err := model.FindUpstreamBalanceVoice(c.Request.Context())
	if err != nil {
		common.ApiErrorT(c, "Unable to load voice alert settings")
		return
	}
	common.ApiSuccess(c, struct {
		model.UpstreamBalanceVoice
		Configured bool `json:"configured"`
	}{config, config.Credential != ""})
}

func SaveUpstreamBalanceVoice(c *gin.Context) {
	var input struct {
		model.UpstreamBalanceVoice
		service.UpstreamBalanceVoiceCredential
	}
	if err := c.ShouldBindJSON(&input); err != nil || !upstreamBalanceVoiceID.MatchString(input.AppID) || !upstreamBalanceVoiceID.MatchString(input.TemplateID) {
		common.ApiErrorT(c, "Enter a valid voice app ID and template ID")
		return
	}
	if !service.UpstreamBalanceEncryptionConfigured() {
		common.ApiErrorT(c, "Set a persistent CRYPTO_SECRET or SESSION_SECRET of at least 32 characters")
		return
	}
	credentials := input.UpstreamBalanceVoiceCredential
	credentials.SecretID = strings.TrimSpace(credentials.SecretID)
	credentials.SecretKey = strings.TrimSpace(credentials.SecretKey)
	var err error
	if credentials.SecretID != "" || credentials.SecretKey != "" {
		if credentials.SecretID == "" || credentials.SecretKey == "" ||
			len(credentials.SecretID) > 256 || len(credentials.SecretKey) > 256 {
			common.ApiErrorT(c, "Enter both the Tencent Cloud Secret ID and Secret Key")
			return
		}
		input.Credential, err = service.EncryptUpstreamBalanceSecret(credentials, service.UpstreamBalanceVoiceBinding)
	} else {
		var saved model.UpstreamBalanceVoice
		saved, err = model.FindUpstreamBalanceVoice(c.Request.Context())
		if err == nil && saved.Credential == "" {
			common.ApiErrorT(c, "Enter both the Tencent Cloud Secret ID and Secret Key")
			return
		}
	}
	if err != nil || model.SaveUpstreamBalanceVoice(c.Request.Context(), input.UpstreamBalanceVoice) != nil {
		common.ApiErrorT(c, "Unable to save voice alert settings")
		return
	}
	recordManageAudit(c, "option.upstream_balance_voice.update", nil)
	common.ApiSuccess(c, nil)
}

func respondUpstreamChannelBalance(c *gin.Context, monitor *model.UpstreamBalanceMonitor) {
	if monitor.BalanceUSD == nil {
		common.ApiErrorT(c, "No upstream account balance yet. Use Query now in the channel settings.")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true, "message": "", "balance": *monitor.BalanceUSD,
		"balance_detail": monitor.BalanceDetail(),
	})
}
