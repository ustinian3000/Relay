package auth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	internalcache "github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
)

type quotaWindowDescription struct {
	Kind          string     `json:"kind"`
	WindowSeconds int64      `json:"window_seconds,omitempty"`
	UsedPercent   *float64   `json:"used_percent,omitempty"`
	Exhausted     bool       `json:"exhausted"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
}

type quotaDescriptionCacheEntry struct {
	mu       sync.Mutex
	expires  time.Time
	snapshot quotaLookupResult
}

// Cache keys hash the credential and effective proxy; neither secret is retained.
var quotaDescriptionCache = internalcache.NewBoundedLRU[string, *quotaDescriptionCacheEntry](256, nil)

func validQuotaPercent(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100
}

func quotaWindowsFromHeaders(provider string, signals map[string]string, now time.Time) []quotaWindowDescription {
	headers := make(http.Header, len(signals))
	for key, value := range signals {
		headers.Set(key, value)
	}
	var windows []quotaWindowDescription
	for _, kind := range []string{"five_hour", "seven_day"} {
		var status, utilization, reset string
		factor := 100.0
		if provider == "claude" {
			prefix := "Anthropic-Ratelimit-Unified-5h-"
			if kind == "seven_day" {
				prefix = "Anthropic-Ratelimit-Unified-7d-"
			}
			status, utilization, reset = headers.Get(prefix+"Status"), headers.Get(prefix+"Utilization"), headers.Get(prefix+"Reset")
		} else if provider == "codex" {
			factor = 1
			for _, slot := range []string{"Primary", "Secondary"} {
				prefix := "X-Codex-" + slot + "-"
				minutes, _ := strconv.Atoi(headers.Get(prefix + "Window-Minutes"))
				if (kind == "five_hour" && minutes == 300) || (kind == "seven_day" && minutes == 10080) {
					utilization = headers.Get(prefix + "Used-Percent")
					reset = headers.Get(prefix + "Reset-At")
				}
			}
		}
		window := quotaWindowDescription{Kind: kind, Exhausted: strings.EqualFold(status, "rejected")}
		if value, err := strconv.ParseFloat(utilization, 64); err == nil && validQuotaPercent(value*factor) {
			value *= factor
			window.UsedPercent = &value
			window.Exhausted = window.Exhausted || value >= 100
		}
		if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil && epoch > 0 {
			instant := time.Unix(epoch, 0).UTC()
			if !instant.After(now) {
				continue
			}
			if instant.Sub(now) <= 8*24*time.Hour {
				window.ResetsAt = &instant
			}
		}
		if window.UsedPercent != nil || status != "" {
			windows = append(windows, window)
		}
	}
	return windows
}

func parseClaudeQuotaUsage(body []byte, now time.Time) []quotaWindowDescription {
	if !ojson.ValidBytes(body) {
		return nil
	}
	var windows []quotaWindowDescription
	for _, kind := range []string{"five_hour", "seven_day"} {
		value := ojson.GetBytes(body, kind+".utilization")
		if value.Type != ojson.Number || !validQuotaPercent(value.Float()) {
			continue
		}
		percent := value.Float()
		window := quotaWindowDescription{Kind: kind, UsedPercent: &percent, Exhausted: percent >= 100}
		if instant, err := time.Parse(time.RFC3339Nano, ojson.GetBytes(body, kind+".resets_at").String()); err == nil {
			if !instant.After(now) {
				continue
			}
			if instant.Sub(now) <= 8*24*time.Hour {
				instant = instant.UTC()
				window.ResetsAt = &instant
			}
		}
		windows = append(windows, window)
	}
	return windows
}

func (entry *quotaDescriptionCacheEntry) load(now time.Time, fetch func() quotaLookupResult) quotaLookupResult {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if now.Before(entry.expires) {
		result := entry.snapshot
		result.source = "cache"
		return result
	}
	entry.snapshot = fetch()
	entry.snapshot.source = "live"
	entry.snapshot.queriedAt = now
	entry.expires = now.Add(time.Minute)
	for _, window := range entry.snapshot.windows {
		if window.ResetsAt != nil && window.ResetsAt.Before(entry.expires) {
			entry.expires = *window.ResetsAt
		}
	}
	return entry.snapshot
}

func (m *Manager) quotaDescriptions(ctx context.Context, account *Auth, now time.Time) []quotaWindowDescription {
	return m.lookupAccountQuota(ctx, account, now, false).windows
}

func humanQuotaWait(duration time.Duration) string {
	seconds := int64(math.Ceil(duration.Seconds()))
	if seconds <= 0 {
		return "不足1秒"
	}
	days, hours, minutes := seconds/86400, (seconds%86400)/3600, (seconds%3600)/60
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%d天", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d小时", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%d分钟", minutes))
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d秒", seconds))
	}
	return strings.Join(parts, "")
}

type quotaDescriptionError struct {
	cause error
	body  string
}

func (e *quotaDescriptionError) Error() string   { return e.body }
func (e *quotaDescriptionError) Unwrap() error   { return e.cause }
func (e *quotaDescriptionError) StatusCode() int { return statusCodeFromError(e.cause) }

// describePoolQuota adds presentation only. It never changes scheduling state,
// recovery deadlines, provider errors, or authorization boundaries.
func (m *Manager) describePoolQuota(ctx context.Context, cause error, pool []*Auth) error {
	var cooldown *modelCooldownError
	if !errors.As(cause, &cooldown) || cooldown == nil {
		return cause
	}
	now := time.Now()
	retryAt := now.Add(cooldown.resetIn)
	displayZone := time.FixedZone("JST", 9*3600)
	summary := fmt.Sprintf("当前账号池额度受限，预计还需等待%s，最早可重试时间：%s（日本时间 UTC+09:00）。", humanQuotaWait(cooldown.resetIn), retryAt.In(displayZone).Format("2006-01-02 15:04:05"))
	message := ""
	accounts := append([]*Auth(nil), pool...)
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	var details []map[string]any
	known := false
	for index, account := range accounts {
		windows := m.quotaDescriptions(ctx, account, now)
		for _, window := range windows {
			label := "五小时额度"
			if window.Kind == "seven_day" {
				label = "周额度"
			} else if window.Kind == "thirty_day" {
				label = "30天额度"
			} else if window.Kind != "five_hour" {
				label = "其他周期额度"
			}
			prefix := ""
			if len(accounts) > 1 {
				prefix = fmt.Sprintf("账号%d：", index+1)
			}
			if window.Exhausted {
				known = true
				message += prefix + label + "已用尽"
				if window.ResetsAt != nil {
					message += fmt.Sprintf("，预计还需等待%s，恢复时间：%s（日本时间）", humanQuotaWait(window.ResetsAt.Sub(now)), window.ResetsAt.In(displayZone).Format("2006-01-02 15:04:05"))
				} else {
					message += "，该额度恢复时间未知"
				}
			} else if window.UsedPercent != nil {
				message += fmt.Sprintf("%s%s已使用%g%%，未耗尽", prefix, label, *window.UsedPercent)
			} else {
				continue
			}
			message += "。"
			details = append(details, map[string]any{"account": index + 1, "window": window})
		}
	}
	if !known {
		message = summary + message + "具体受限额度类型未知，请稍后重试。"
	} else {
		message += fmt.Sprintf("建议于%s（日本时间）后重试。", retryAt.In(displayZone).Format("2006-01-02 15:04:05"))
	}
	doc := ojson.NewDocString(cooldown.Error())
	_ = doc.Set("error.message", message)
	_ = doc.Set("error.resets_at", retryAt.UTC().Format(time.RFC3339))
	_ = doc.Set("error.timezone", "Asia/Tokyo")
	if len(details) > 0 {
		_ = doc.Set("error.quota_windows", details)
	}
	return &quotaDescriptionError{cause: cause, body: doc.String()}
}
