package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

type AccountUsageError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
}

type quotaLookupResult struct {
	windows              []quotaWindowDescription
	queriedAt            time.Time
	source               string
	queryError           *AccountUsageError
	providerAllowed      *bool
	providerLimitReached bool
}

func quotaQueryFailure(code, message string, status int) quotaLookupResult {
	return quotaLookupResult{queryError: &AccountUsageError{Code: code, Message: message, HTTPStatus: status}}
}

func parseCodexQuotaUsage(body []byte, now time.Time) []quotaWindowDescription {
	if !ojson.ValidBytes(body) {
		return nil
	}
	var windows []quotaWindowDescription
	for _, slot := range []string{"primary_window", "secondary_window"} {
		prefix := "rate_limit." + slot + "."
		period := ojson.GetBytes(body, prefix+"limit_window_seconds")
		if period.Type != ojson.Number || period.Float() <= 0 || period.Float() > 366*86400 || math.Trunc(period.Float()) != period.Float() {
			continue
		}
		seconds := period.Int()
		kind := ""
		switch seconds {
		case 18000:
			kind = "five_hour"
		case 604800:
			kind = "seven_day"
		case 2592000:
			kind = "thirty_day"
		default:
			kind = "custom"
		}
		used := ojson.GetBytes(body, prefix+"used_percent")
		if used.Type != ojson.Number || !validQuotaPercent(used.Float()) {
			continue
		}
		percent := used.Float()
		window := quotaWindowDescription{Kind: kind, WindowSeconds: seconds, UsedPercent: &percent, Exhausted: percent >= 100}
		epoch := ojson.GetBytes(body, prefix+"reset_at").Int()
		var instant time.Time
		if epoch > 0 {
			instant = time.Unix(epoch, 0).UTC()
		} else {
			after := ojson.GetBytes(body, prefix+"reset_after_seconds")
			if after.Type == ojson.Number && after.Float() >= 0 && after.Float() <= float64(seconds+86400) {
				instant = now.Add(time.Duration(math.Ceil(after.Float())) * time.Second).UTC()
			}
		}
		if !instant.IsZero() {
			if !instant.After(now) {
				continue
			}
			if instant.Sub(now) <= time.Duration(seconds+86400)*time.Second {
				window.ResetsAt = &instant
			}
		}
		windows = append(windows, window)
	}
	return windows
}

// fetchAccountQuota sends only a fixed read-only provider request. Arbitrary
// upstream errors and bodies never enter the public account-usage response.
func fetchAccountQuota(ctx context.Context, client *http.Client, account *Auth, now time.Time) quotaLookupResult {
	token, _ := account.Metadata["access_token"].(string)
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	target := "https://api.anthropic.com/api/oauth/usage"
	if provider == "codex" {
		target = "https://chatgpt.com/backend-api/wham/usage"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return quotaQueryFailure("query_unavailable", "Quota query could not be prepared", 0)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if provider == "claude" {
		request.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
		request.Header.Set("User-Agent", "claude-code/2.1.220")
	} else {
		accountID, _ := account.Metadata["account_id"].(string)
		if accountID == "" {
			return quotaQueryFailure("missing_account_id", "Codex account identity is unavailable", 0)
		}
		request.Header.Set("Chatgpt-Account-Id", accountID)
		request.Header.Set("User-Agent", "codex_cli_rs")
	}
	response, err := client.Do(request)
	if err != nil {
		return quotaQueryFailure("upstream_unreachable", "Quota provider could not be reached through the configured connection", 0)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.Debug("failed to close quota query response")
		}
	}()
	if response.StatusCode != http.StatusOK {
		code := "quota_query_failed"
		switch response.StatusCode {
		case 401:
			code = "upstream_unauthorized"
		case 403:
			code = "upstream_forbidden"
		case 429:
			code = "quota_query_rate_limited"
		}
		return quotaQueryFailure(code, "Quota provider rejected the read-only query", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		return quotaQueryFailure("invalid_quota_response", "Quota provider returned an unreadable response", 0)
	}
	windows := parseClaudeQuotaUsage(body, now)
	if provider == "codex" {
		windows = parseCodexQuotaUsage(body, now)
	}
	if len(windows) == 0 {
		return quotaQueryFailure("quota_data_unavailable", "Provider did not return recognized quota windows", 0)
	}
	result := quotaLookupResult{windows: windows}
	if provider == "codex" {
		allowed := ojson.GetBytes(body, "rate_limit.allowed")
		if allowed.Type == ojson.True || allowed.Type == ojson.False {
			value := allowed.Bool()
			result.providerAllowed = &value
		}
		result.providerLimitReached = ojson.GetBytes(body, "rate_limit.limit_reached").Bool()
	}
	return result
}

func (m *Manager) lookupAccountQuota(ctx context.Context, account *Auth, now time.Time, preferLive bool) quotaLookupResult {
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	observed := quotaLookupResult{windows: quotaWindowsFromHeaders(provider, account.Quota.Signals, now), queriedAt: account.Quota.ObservedAt, source: "observed"}
	if !preferLive && len(observed.windows) > 0 {
		return observed
	}
	if provider != "claude" && provider != "codex" {
		return quotaQueryFailure("unsupported_provider", "Live quota queries are not supported for this provider", 0)
	}
	token, _ := account.Metadata["access_token"].(string)
	if token == "" {
		if len(observed.windows) > 0 {
			return observed
		}
		return quotaQueryFailure("missing_access_token", "No OAuth access token is available for quota queries", 0)
	}
	proxy := account.ProxyURL
	if proxy == "" {
		if config := m.runtimeConfigSnapshot(); config != nil {
			proxy = config.ProxyURL
		}
	}
	accountID, _ := account.Metadata["account_id"].(string)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(provider+"\x00"+token+"\x00"+accountID+"\x00"+proxy)))
	entry := quotaDescriptionCache.GetOrAdd(key, func() *quotaDescriptionCacheEntry { return &quotaDescriptionCacheEntry{} })
	result := entry.load(now, func() quotaLookupResult {
		transportAccount := account.Clone()
		transportAccount.ProxyURL = proxy
		transport := m.roundTripperFor(transportAccount)
		var client *http.Client
		if transport != nil {
			client = &http.Client{Transport: transport}
		} else {
			owned, _, errBuild := proxyutil.BuildHTTPTransport(proxy)
			if errBuild != nil {
				return quotaQueryFailure("invalid_proxy", "Configured proxy could not be used; direct fallback is disabled", 0)
			}
			if owned == nil {
				owned = proxyutil.NewDirectTransport()
			}
			client = &http.Client{Transport: owned}
			defer client.CloseIdleConnections()
		}
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return fetchAccountQuota(ctx, client, account, now)
	})
	if result.queryError != nil && len(observed.windows) > 0 {
		observed.queryError = result.queryError
		return observed
	}
	return result
}
