package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	tencent "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	tencenterrors "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/errors"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	vms "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/vms/v20200902"
)

const UpstreamBalanceVoiceBinding = "upstream-balance/tencent-vms"

type UpstreamBalanceVoiceCredential struct {
	SecretID  string `json:"secret_id"`
	SecretKey string `json:"secret_key"`
}

var upstreamBalanceVoiceTransport = http.DefaultTransport

func sendUpstreamBalanceVoice(ctx context.Context, config model.UpstreamBalanceVoice, auth UpstreamBalanceVoiceCredential, phone string) (string, string) {
	clientProfile := profile.NewClientProfile()
	clientProfile.HttpProfile.ReqTimeout = 10
	clientProfile.DisableRegionBreaker = true
	clientProfile.NetworkFailureMaxRetries = 0
	clientProfile.RateLimitExceededMaxRetries = 0
	client, err := vms.NewClient(tencent.NewCredential(auth.SecretID, auth.SecretKey), "", clientProfile)
	if err != nil {
		return "rejected", ""
	}
	req := vms.NewSendTtsVoiceRequest()
	req.VoiceSdkAppid = &config.AppID
	req.TemplateId = &config.TemplateID
	req.CalledNumber = &phone
	req.TemplateParamSet = []*string{}
	client.WithHttpTransport(upstreamBalanceVoiceTransport)
	response, err := client.SendTtsVoiceWithContext(ctx, req)
	if err != nil {
		var apiError *tencenterrors.TencentCloudSDKError
		if errors.As(err, &apiError) && (strings.HasPrefix(apiError.Code, "AuthFailure") ||
			strings.HasPrefix(apiError.Code, "InvalidParameter") || strings.HasPrefix(apiError.Code, "LimitExceeded") ||
			strings.HasPrefix(apiError.Code, "UnauthorizedOperation") || apiError.Code == "FailedOperation.TemplateIncorrectOrUnapproved") {
			return "rejected", ""
		}
		return "unknown", ""
	}
	if response == nil || response.Response == nil || response.Response.SendStatus == nil ||
		response.Response.SendStatus.CallId == nil || *response.Response.SendStatus.CallId == "" ||
		len(*response.Response.SendStatus.CallId) > 128 {
		return "unknown", ""
	}
	return "submitted", *response.Response.SendStatus.CallId
}

func RunUpstreamBalanceMonitors(ctx context.Context, report func(int, int)) (int, error) {
	if err := model.DeleteOrphanedUpstreamBalanceMonitors(ctx); err != nil {
		return 0, errUpstreamStorage
	}
	monitors, err := model.FindDueUpstreamBalanceMonitors(ctx, common.GetTimestamp())
	if err != nil {
		return 0, errUpstreamStorage
	}
	var failed bool
	for index := range monitors {
		if err := ctx.Err(); err != nil {
			return index, err
		}
		if err := runUpstreamBalanceMonitor(ctx, upstreamBalanceHTTP, &monitors[index]); err != nil {
			failed = true
		}
		report(index+1, len(monitors))
	}
	if failed {
		return len(monitors), errUpstreamStorage
	}
	return len(monitors), nil
}

func runUpstreamBalanceMonitor(ctx context.Context, client *http.Client, monitor *model.UpstreamBalanceMonitor) error {
	now := common.GetTimestamp()
	doQuery := monitor.RequestedAt > monitor.QueriedRequest || monitor.NextQueryAt > 0 && monitor.NextQueryAt <= now
	doRefresh := monitor.NextRefreshAt > 0 && monitor.NextRefreshAt <= now
	var credential UpstreamBalanceCredential
	decryptErr := decryptUpstreamBalanceSecret(monitor.Credential, UpstreamBalanceSecretBinding(monitor), &credential)
	accessExpiresSoon := monitor.AccessExpiresAt == 0 || monitor.AccessExpiresAt <= now+90
	if doRefresh || (doQuery && monitor.RefreshMinutes > 0 && accessExpiresSoon && monitor.NextRefreshAt > 0) {
		updates := map[string]any{"next_refresh_at": monitor.NextRefresh(now, monitor.AccessExpiresAt)}
		switch {
		case decryptErr != nil:
			updates["refresh_error"] = upstreamStorageError
		case monitor.RefreshError == "refreshing":
			updates["refresh_error"] = "refresh_unknown"
			updates["next_refresh_at"] = 0
		case credential.Refresh == "":
			updates["refresh_error"] = upstreamCredentialError
			updates["next_refresh_at"] = 0
		case accessExpiresSoon || credential.Access == "":
			claimed, err := monitor.ClaimRefresh(ctx)
			if err != nil || !claimed {
				return err
			}
			next, expiry, err := refreshUpstreamAccount(ctx, client, monitor, credential, now)
			if err != nil {
				updates["next_refresh_at"] = 0
				updates["refresh_error"] = "refresh_unknown"
				if errors.Is(err, errUpstreamCredential) {
					updates["refresh_error"] = upstreamCredentialError
				}
			} else {
				sealed, err := EncryptUpstreamBalanceSecret(next, UpstreamBalanceSecretBinding(monitor))
				if err != nil {
					return err
				}
				updates["credential"] = sealed
				updates["session_key"] = UpstreamBalanceSessionKey(monitor, next)
				updates["access_expires_at"] = expiry
				updates["next_refresh_at"] = monitor.NextRefresh(now, expiry)
				updates["refresh_error"] = ""
				credential = next
			}
		}
		stored, err := monitor.StoreRefresh(ctx, updates)
		if err != nil || !stored {
			return err
		}
	}
	if !doQuery {
		return nil
	}

	var amount *float64
	queryError := ""
	if decryptErr != nil {
		queryError = upstreamStorageError
	} else {
		balance, err := queryUpstreamAccountBalance(ctx, client, monitor, credential)
		if err != nil {
			queryError = err.Error()
		} else {
			amount = &balance
		}
	}
	stored, err := monitor.StoreQuery(ctx, now, amount, queryError)
	if err != nil || !stored || amount == nil || !monitor.AlertEnabled {
		return err
	}
	return checkUpstreamBalanceAlert(ctx, monitor, *amount)
}

func checkUpstreamBalanceAlert(ctx context.Context, monitor *model.UpstreamBalanceMonitor, amount float64) error {
	state := monitor.AlertState
	if amount > monitor.ThresholdUSD {
		if state == "" {
			return nil
		}
		_, err := monitor.StoreAlert(ctx, state, "", "")
		return err
	}
	if state == "submitted" || state == "sending" || state == "unknown" {
		return nil
	}
	config, err := model.FindUpstreamBalanceVoice(ctx)
	if err != nil {
		return err
	}
	var auth UpstreamBalanceVoiceCredential
	if config.Credential == "" || decryptUpstreamBalanceSecret(config.Credential, UpstreamBalanceVoiceBinding, &auth) != nil {
		_, err := monitor.StoreAlert(ctx, state, "unconfigured", "")
		return err
	}
	// Reserve the call before sending. A transport timeout or restart may leave the call accepted upstream.
	claimed, err := monitor.StoreAlert(ctx, state, "sending", "")
	if err != nil || !claimed {
		return err
	}
	result, callID := sendUpstreamBalanceVoice(ctx, config, auth, monitor.Phone)
	_, err = monitor.StoreAlert(ctx, "sending", result, callID)
	return err
}
