package model

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type UpstreamBalanceMonitor struct {
	ChannelID       int      `gorm:"primaryKey;autoIncrement:false" json:"-"`
	Provider        string   `gorm:"size:24" json:"provider"`
	BaseURL         string   `gorm:"type:text" json:"base_url"`
	UpstreamUserID  int      `json:"upstream_user_id"`
	Credential      string   `gorm:"type:text" json:"-"`
	SessionKey      *string  `gorm:"size:64;uniqueIndex" json:"-"`
	Version         string   `gorm:"size:36" json:"-"`
	QueryMinutes    int      `json:"query_minutes"`
	RefreshMinutes  int      `json:"refresh_minutes"`
	AlertEnabled    bool     `json:"alert_enabled"`
	ThresholdUSD    float64  `json:"threshold_usd"`
	Phone           string   `gorm:"size:16" json:"phone"`
	BalanceUSD      *float64 `json:"balance_usd"`
	CheckedAt       int64    `json:"checked_at"`
	QueryError      string   `gorm:"size:32" json:"query_error,omitempty"`
	RefreshError    string   `gorm:"size:32" json:"refresh_error,omitempty"`
	AccessExpiresAt int64    `json:"access_expires_at"`
	NextQueryAt     int64    `gorm:"index" json:"-"`
	NextRefreshAt   int64    `gorm:"index" json:"-"`
	RequestedAt     int64    `json:"-"`
	QueriedRequest  int64    `json:"-"`
	AlertState      string   `gorm:"size:24" json:"alert_state,omitempty"`
	AlertCallID     string   `gorm:"size:128" json:"alert_call_id,omitempty"`
}

type UpstreamBalanceVoice struct {
	ID         int    `gorm:"primaryKey;autoIncrement:false" json:"-"`
	Credential string `gorm:"type:text" json:"-"`
	AppID      string `gorm:"size:32" json:"app_id"`
	TemplateID string `gorm:"size:32" json:"template_id"`
}

func FindUpstreamBalanceMonitor(ctx context.Context, channelID int) (*UpstreamBalanceMonitor, error) {
	var monitor UpstreamBalanceMonitor
	err := DB.WithContext(ctx).First(&monitor, "channel_id = ?", channelID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &monitor, err
}

const upstreamBalanceDue = "(next_query_at > 0 AND next_query_at <= ?) OR (next_refresh_at > 0 AND next_refresh_at <= ?) OR requested_at > queried_request"

func HasDueUpstreamBalanceMonitors(now int64) (bool, error) {
	var monitor UpstreamBalanceMonitor
	err := DB.Select("channel_id").Where(upstreamBalanceDue, now, now).
		Or("channel_id NOT IN (?)", DB.Model(&Channel{}).Select("id")).Take(&monitor).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}

func FindDueUpstreamBalanceMonitors(ctx context.Context, now int64) ([]UpstreamBalanceMonitor, error) {
	var monitors []UpstreamBalanceMonitor
	err := DB.WithContext(ctx).
		Where(upstreamBalanceDue, now, now).
		Order("channel_id").Find(&monitors).Error
	return monitors, err
}

func SaveUpstreamBalanceMonitor(ctx context.Context, monitor *UpstreamBalanceMonitor, oldVersion string, replaceCredentials bool) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var channel Channel
		if err := lockForUpdate(tx).Select("id", "other_info").First(&channel, monitor.ChannelID).Error; err != nil {
			return err
		}
		if oldVersion == "" {
			if err := tx.Create(monitor).Error; err != nil {
				return err
			}
		} else {
			updates := map[string]any{
				"version": monitor.Version, "provider": monitor.Provider, "base_url": monitor.BaseURL,
				"upstream_user_id": monitor.UpstreamUserID, "query_minutes": monitor.QueryMinutes,
				"refresh_minutes": monitor.RefreshMinutes, "alert_enabled": monitor.AlertEnabled,
				"threshold_usd": monitor.ThresholdUSD, "phone": monitor.Phone,
				"next_query_at": monitor.NextQueryAt, "next_refresh_at": monitor.NextRefreshAt,
			}
			if replaceCredentials {
				updates["credential"] = monitor.Credential
				updates["session_key"] = monitor.SessionKey
				updates["access_expires_at"] = monitor.AccessExpiresAt
				updates["balance_usd"] = nil
				updates["checked_at"] = 0
				updates["query_error"] = ""
				updates["refresh_error"] = ""
			}
			if replaceCredentials || !monitor.AlertEnabled {
				updates["alert_state"] = ""
				updates["alert_call_id"] = ""
			}
			result := tx.Model(monitor).Where("version = ?", oldVersion).Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return gorm.ErrRecordNotFound
			}
		}
		if replaceCredentials {
			return writeChannelUpstreamBalance(tx, &channel, monitor.BalanceDetail(), 0)
		}
		return nil
	})
}

func RemoveUpstreamBalanceMonitor(ctx context.Context, channelID int) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var channel Channel
		if err := lockForUpdate(tx).Select("id", "other_info").First(&channel, channelID).Error; err != nil {
			return err
		}
		result := tx.Delete(&UpstreamBalanceMonitor{}, "channel_id = ?", channelID)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		return writeChannelUpstreamBalance(tx, &channel, nil, 0)
	})
}

func RequestUpstreamBalance(ctx context.Context, channelID int) (bool, error) {
	result := DB.WithContext(ctx).Model(&UpstreamBalanceMonitor{}).Where("channel_id = ?", channelID).
		Update("requested_at", time.Now().UnixNano())
	return result.RowsAffected > 0, result.Error
}

func (m *UpstreamBalanceMonitor) ClaimRefresh(ctx context.Context) (bool, error) {
	result := DB.WithContext(ctx).Model(&UpstreamBalanceMonitor{}).
		Where("channel_id = ? AND credential = ? AND next_refresh_at > 0 AND refresh_error <> ?", m.ChannelID, m.Credential, "refreshing").
		Update("refresh_error", "refreshing")
	return result.RowsAffected > 0, result.Error
}

func (m *UpstreamBalanceMonitor) StoreRefresh(ctx context.Context, updates map[string]any) (bool, error) {
	if next, ok := updates["next_refresh_at"]; ok {
		updates["next_refresh_at"] = gorm.Expr("CASE WHEN refresh_minutes > 0 THEN ? ELSE 0 END", next)
	}
	result := DB.WithContext(ctx).Model(&UpstreamBalanceMonitor{}).Where("channel_id = ? AND credential = ?", m.ChannelID, m.Credential).Updates(updates)
	return result.RowsAffected > 0, result.Error
}

func (m *UpstreamBalanceMonitor) StoreQuery(ctx context.Context, checkedAt int64, amount *float64, queryError string) (bool, error) {
	var stored bool
	updates := map[string]any{
		"query_error": queryError, "next_query_at": m.NextQuery(checkedAt), "queried_request": m.RequestedAt,
	}
	if amount != nil {
		updates["checked_at"] = checkedAt
		updates["balance_usd"] = *amount
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var channel Channel
		if err := lockForUpdate(tx).Select("id", "other_info").First(&channel, m.ChannelID).Error; err != nil {
			return err
		}
		result := tx.Model(&UpstreamBalanceMonitor{}).Where("channel_id = ? AND version = ?", m.ChannelID, m.Version).Updates(updates)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		stored = true
		if amount != nil {
			detail := m.BalanceDetail()
			detail.Amount = amount
			return writeChannelUpstreamBalance(tx, &channel, detail, checkedAt)
		}
		return nil
	})
	return stored, err
}

func (m *UpstreamBalanceMonitor) StoreAlert(ctx context.Context, expected, state, callID string) (bool, error) {
	result := DB.WithContext(ctx).Model(&UpstreamBalanceMonitor{}).Where("channel_id = ? AND version = ? AND alert_state = ?", m.ChannelID, m.Version, expected).
		Updates(map[string]any{"alert_state": state, "alert_call_id": callID})
	return result.RowsAffected > 0, result.Error
}

func (m *UpstreamBalanceMonitor) BalanceDetail() *ChannelBalanceDetail {
	return &ChannelBalanceDetail{
		UpstreamAccount: true,
		BalanceResult:   jsplugin.BalanceResult{Amount: m.BalanceUSD, Currency: "USD", Windows: []jsplugin.BalanceWindow{}},
	}
}

func writeChannelUpstreamBalance(tx *gorm.DB, channel *Channel, detail *ChannelBalanceDetail, updatedAt int64) error {
	info := channel.GetOtherInfo()
	if info == nil {
		info = make(map[string]any)
	}
	var balance float64
	if detail == nil {
		delete(info, channelBalanceDetailKey)
	} else {
		info[channelBalanceDetailKey] = detail
		if detail.Amount != nil {
			balance = *detail.Amount
		}
	}
	channel.SetOtherInfo(info)
	return tx.Model(channel).Updates(map[string]any{
		"other_info": channel.OtherInfo, "balance": balance, "balance_updated_time": updatedAt,
	}).Error
}

func UpstreamBalanceSessionInUse(ctx context.Context, key string, channelID int) bool {
	var monitor UpstreamBalanceMonitor
	return DB.WithContext(ctx).Select("channel_id").
		Where("session_key = ? AND channel_id <> ?", key, channelID).Take(&monitor).Error == nil
}

func FindUpstreamBalanceVoice(ctx context.Context) (UpstreamBalanceVoice, error) {
	var config UpstreamBalanceVoice
	err := DB.WithContext(ctx).First(&config, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = nil
	}
	return config, err
}

func SaveUpstreamBalanceVoice(ctx context.Context, config UpstreamBalanceVoice) error {
	columns := []string{"app_id", "template_id"}
	if config.Credential != "" {
		columns = append(columns, "credential")
	}
	config.ID = 1
	return DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns(columns),
	}).Create(&config).Error
}

func DeleteOrphanedUpstreamBalanceMonitors(ctx context.Context) error {
	return DB.WithContext(ctx).
		Where("channel_id NOT IN (?)", DB.Model(&Channel{}).Select("id")).
		Delete(&UpstreamBalanceMonitor{}).Error
}

func (m *UpstreamBalanceMonitor) NextQuery(now int64) int64 {
	if m.QueryMinutes <= 0 {
		return 0
	}
	return now + int64(m.QueryMinutes)*60
}

func (m *UpstreamBalanceMonitor) NextRefresh(now, expiresAt int64) int64 {
	if m.RefreshMinutes <= 0 {
		return 0
	}
	next := now + int64(m.RefreshMinutes)*60
	if expiresAt > now {
		return min(next, max(now+15, expiresAt-60))
	}
	return next
}

func QueueUpstreamBalanceOnSave(monitor *UpstreamBalanceMonitor) {
	now := common.GetTimestamp()
	if monitor.QueryMinutes > 0 {
		monitor.NextQueryAt = now
	}
	if monitor.RefreshMinutes > 0 {
		monitor.NextRefreshAt = now
	}
}
