package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type AccountUsageWindow struct {
	UsedPercent      *float64   `json:"usedPercent"`
	RemainingPercent *float64   `json:"remainingPercent"`
	Exhausted        bool       `json:"exhausted"`
	ResetsAt         *time.Time `json:"resetsAt"`
	ResetsInSeconds  *int64     `json:"resetsInSeconds"`
}

type AccountUsage struct {
	ID              string               `json:"id"`
	Provider        string               `json:"provider"`
	Status          string               `json:"status"`
	Available       *bool                `json:"available"`
	FiveHour        *AccountUsageWindow  `json:"fiveHour"`
	Weekly          *AccountUsageWindow  `json:"weekly"`
	Monthly         *AccountUsageWindow  `json:"monthly"`
	Windows         []AccountUsagePeriod `json:"windows"`
	CooldownSeconds *int64               `json:"cooldownSeconds"`
	CooldownUntil   *time.Time           `json:"cooldownUntil"`
	QueriedAt       *time.Time           `json:"queriedAt"`
	Source          string               `json:"source"`
	Error           *AccountUsageError   `json:"error"`
}

type AccountUsagePeriod struct {
	Kind            string              `json:"kind"`
	DurationSeconds int64               `json:"durationSeconds"`
	Quota           *AccountUsageWindow `json:"quota"`
}

type AccountUsageResult struct {
	Accounts  []AccountUsage `json:"accounts"`
	QueriedAt time.Time      `json:"queriedAt"`
}

// AccountUsage authorizes the full inventory before any upstream quota request.
// It does not run a selector, refresh credentials, or mutate cooldown state.
func (m *Manager) AccountUsage(ctx context.Context, callerScope string) (AccountUsageResult, error) {
	result := AccountUsageResult{Accounts: make([]AccountUsage, 0)}
	if m == nil || strings.TrimSpace(callerScope) == "" || m.HomeEnabled() {
		return result, fmt.Errorf("local account pool authorization is required")
	}
	scheduler, _ := m.snapshotPluginScheduler()
	pool := m.snapshotAuths()
	accounts, err := inspectAuthorizedAuthPool(ctx, scheduler, pluginapi.SchedulerPickRequest{
		Options: pluginapi.SchedulerOptions{Metadata: map[string]any{pluginapi.CallerScopeMetadataKey: callerScope}},
	}, pool)
	if err != nil {
		return result, err
	}
	result.Accounts = make([]AccountUsage, len(accounts))
	jobs := make(chan int, len(accounts))
	for index := range accounts {
		jobs <- index
	}
	close(jobs)
	workers := len(accounts)
	if workers > 4 {
		workers = 4
	}
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				result.Accounts[index] = m.accountUsage(ctx, accounts[index], callerScope)
			}
		}()
	}
	group.Wait()
	if err := ctx.Err(); err != nil {
		return AccountUsageResult{}, err
	}
	sort.Slice(result.Accounts, func(i, j int) bool { return result.Accounts[i].ID < result.Accounts[j].ID })
	result.QueriedAt = time.Now().UTC()
	return result, nil
}

func (m *Manager) accountUsage(ctx context.Context, account *Auth, scope string) AccountUsage {
	now := time.Now()
	id := sha256.Sum256([]byte("account-usage:v1\x00" + scope + "\x00" + account.ID))
	row := AccountUsage{ID: fmt.Sprintf("account-%x", id[:8]), Provider: strings.ToLower(account.Provider), Status: "unknown", Source: "unavailable", Windows: make([]AccountUsagePeriod, 0)}
	lookup := quotaLookupResult{}
	if account.Disabled || account.Status == StatusDisabled || account.RequiresSignIn {
		lookup.windows = quotaWindowsFromHeaders(row.Provider, account.Quota.Signals, now)
		lookup.queriedAt = account.Quota.ObservedAt
		lookup.source = "observed"
	} else {
		lookup = m.lookupAccountQuota(ctx, account, now, true)
	}
	if lookup.source != "" {
		row.Source = lookup.source
	}
	row.Error = lookup.queryError
	if !lookup.queriedAt.IsZero() {
		instant := lookup.queriedAt.UTC()
		row.QueriedAt = &instant
	}
	var recovery time.Time
	exhausted, unknownRecovery := false, false
	for _, window := range lookup.windows {
		mapped := &AccountUsageWindow{UsedPercent: window.UsedPercent, Exhausted: window.Exhausted, ResetsAt: window.ResetsAt}
		if window.UsedPercent != nil {
			remaining := 100 - *window.UsedPercent
			mapped.RemainingPercent = &remaining
		}
		if window.ResetsAt != nil {
			seconds := int64(math.Max(0, math.Ceil(window.ResetsAt.Sub(now).Seconds())))
			mapped.ResetsInSeconds = &seconds
		}
		if window.Kind == "five_hour" {
			row.FiveHour = mapped
		} else if window.Kind == "seven_day" {
			row.Weekly = mapped
		} else if window.Kind == "thirty_day" {
			row.Monthly = mapped
		}
		duration := window.WindowSeconds
		if duration == 0 {
			switch window.Kind {
			case "five_hour":
				duration = 18000
			case "seven_day":
				duration = 604800
			}
		}
		row.Windows = append(row.Windows, AccountUsagePeriod{Kind: window.Kind, DurationSeconds: duration, Quota: mapped})
		if window.Exhausted {
			exhausted = true
			if window.ResetsAt == nil {
				unknownRecovery = true
			} else if window.ResetsAt.After(recovery) {
				recovery = *window.ResetsAt
			}
		}
	}
	runtimeDeadline := account.NextRetryAfter
	if account.Quota.NextRecoverAt.After(runtimeDeadline) {
		runtimeDeadline = account.Quota.NextRecoverAt
	}
	runtimeBlocked := account.Unavailable || account.Quota.Exceeded
	if runtimeBlocked && runtimeDeadline.After(now) && runtimeDeadline.After(recovery) {
		recovery = runtimeDeadline
	}
	available := false
	switch {
	case account.Disabled || account.Status == StatusDisabled:
		row.Status = "disabled"
		row.Available = &available
	case account.RequiresSignIn || (lookup.queryError != nil && lookup.queryError.HTTPStatus == 401):
		row.Status = "authentication_failed"
		row.Available = &available
		if row.Error == nil {
			row.Error = &AccountUsageError{Code: "credential_requires_sign_in", Message: "Credential must be authorized again"}
		}
	case exhausted || lookup.providerLimitReached:
		row.Status = "quota_exhausted"
		row.Available = &available
	case runtimeBlocked && (runtimeDeadline.IsZero() || runtimeDeadline.After(now)):
		row.Status = "cooling_down"
		row.Available = &available
	case lookup.providerAllowed != nil && !*lookup.providerAllowed:
		row.Status = "unavailable"
		row.Available = &available
	case lookup.queryError != nil:
		row.Status = "query_failed"
		if lookup.queryError.Code == "unsupported_provider" {
			row.Status = "unsupported"
		}
	case len(lookup.windows) > 0:
		row.Status = "ok"
		if ((row.FiveHour != nil && row.Weekly != nil) || (lookup.providerAllowed != nil && *lookup.providerAllowed)) && !lookup.queriedAt.IsZero() && now.Sub(lookup.queriedAt) <= time.Minute {
			available = true
			row.Available = &available
			zero := int64(0)
			row.CooldownSeconds = &zero
		}
	}
	if (row.Status == "quota_exhausted" || row.Status == "cooling_down") && recovery.After(now) && (!unknownRecovery || (runtimeBlocked && runtimeDeadline.After(now))) {
		instant := recovery.UTC()
		seconds := int64(math.Ceil(recovery.Sub(now).Seconds()))
		row.CooldownUntil = &instant
		row.CooldownSeconds = &seconds
	}
	return row
}
