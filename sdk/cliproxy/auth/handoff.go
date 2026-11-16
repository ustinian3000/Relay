package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/handoff"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func strictHandoff(opts coreexecutor.Options) bool {
	enabled, _ := opts.Metadata[coreexecutor.StrictHandoffMetadataKey].(bool)
	return enabled
}

// HandoffAccounts applies the same fail-closed pool authorization as dispatch
// before reading any quota. Raw auth IDs remain internal to the server.
func (m *Manager) HandoffAccounts(ctx context.Context, scope string, models []string) ([]handoff.Account, error) {
	if m == nil || scope == "" || m.HomeEnabled() {
		return nil, fmt.Errorf("local authorized pool required")
	}
	scheduler, _ := m.snapshotPluginScheduler()
	pool, err := inspectAuthorizedAuthPool(ctx, scheduler, pluginapi.SchedulerPickRequest{
		Options: pluginapi.SchedulerOptions{Metadata: map[string]any{pluginapi.CallerScopeMetadataKey: scope}},
	}, m.snapshotAuths())
	if err != nil {
		return nil, err
	}
	result := make([]handoff.Account, 0, len(pool))
	for _, a := range pool {
		if strings.ToLower(a.Provider) != "claude" {
			continue
		}
		u := m.accountUsage(ctx, a, scope)
		row := handoff.Account{ID: a.ID, Available: u.Available != nil && *u.Available && !a.Disabled && !a.RequiresSignIn}
		if u.FiveHour != nil && u.Weekly != nil && u.FiveHour.UsedPercent != nil && u.Weekly.UsedPercent != nil {
			row.Known = true
			row.FiveUsed = *u.FiveHour.UsedPercent
			row.WeekUsed = *u.Weekly.UsedPercent
		}
		for _, model := range models {
			if m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
				row.Models = append(row.Models, model)
			}
		}
		result = append(result, row)
	}
	return result, ctx.Err()
}
