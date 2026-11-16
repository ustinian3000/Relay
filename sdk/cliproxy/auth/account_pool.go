package auth

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type accountPoolInspector interface {
	InspectAuthPool(context.Context, pluginapi.SchedulerPickRequest) ([]string, bool, error)
}

// inspectAuthorizedAuthPool shares the same fail-closed authorization boundary
// between account diagnostics and session-aware dispatch.
func inspectAuthorizedAuthPool(ctx context.Context, scheduler PluginScheduler, req pluginapi.SchedulerPickRequest, pool []*Auth) ([]*Auth, error) {
	inspector, ok := scheduler.(accountPoolInspector)
	if !ok {
		return nil, fmt.Errorf("account pool inspection is unavailable")
	}
	req.InspectPool = true
	req.Candidates = nil
	req.PoolCandidates = schedulerAuthCandidates(pool)
	ids, handled, err := inspector.InspectAuthPool(ctx, req)
	if err != nil || !handled {
		return nil, fmt.Errorf("account pool inspection was not authorized")
	}
	accounts := make([]*Auth, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		account := pickSchedulerAuthByID(pool, id)
		if account == nil {
			return nil, fmt.Errorf("account pool inspection returned an unknown credential")
		}
		if !seen[id] {
			seen[id] = true
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}
