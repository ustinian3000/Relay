package main

import (
	"encoding/json"
	"fmt"
)

const pluginName = "key-account-bind"

// pluginVersion is overridden at release time via -ldflags -X main.pluginVersion=...
var pluginVersion = "0.3.0-local-account-usage.4"

// handleConfigure implements plugin.register / plugin.reconfigure.
func handleConfigure(req []byte) []byte {
	state.Store(nil)
	var lr lifecycleRequest
	if len(req) > 0 {
		if err := json.Unmarshal(req, &lr); err != nil {
			return errorEnvelope("invalid_request", "bad lifecycle payload: "+err.Error())
		}
	}
	p, err := parsePolicy(lr.ConfigYAML)
	if err != nil {
		return errorEnvelope("invalid_config", err.Error())
	}
	state.Store(p)
	selections.reset()
	return okEnvelope(registrationResponse{
		SchemaVersion: 7,
		Metadata: registrationMetadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "FFatTiger",
			GitHubRepository: "https://github.com/FFatTiger/cpa-key-account-bind",
			Description:      "Bind authenticated caller scopes to upstream credential pools; requires CLIProxyAPI caller_scope metadata.",
			ConfigFields: []configField{
				{
					Name:        "bindings",
					Type:        "array",
					Description: "Per-key bindings: key=allowGlob1,allowGlob2. Keys are case-sensitive; auth ID patterns are case-insensitive.",
				},
				{
					Name:        "unbound",
					Type:        "enum",
					EnumValues:  []string{"passthrough", "deny"},
					Description: "What to do with authenticated callers without a binding. Default deny; passthrough explicitly permits the full host pool.",
				},
				{
					Name:        "strategy",
					Type:        "enum",
					EnumValues:  []string{strategyRoundRobin, strategyWeightedRoundRobin, strategyFillFirst},
					Description: "Scheduling strategy inside each key's allowed credential set. Mirrors CPA routing.strategy; default is round-robin.",
				},
			},
		},
		Capabilities: registrationCapability{Scheduler: true, SchedulerAcrossPriorities: true, SchedulerPoolDiagnostics: true, SchedulerPoolInspection: true, FrontendAuthProvider: true, FrontendAuthProviderExclusive: true, FrontendAuthProviderOmitBody: true},
	})
}

func handlePick(req []byte) []byte {
	var pr schedulerPickRequest
	if err := json.Unmarshal(req, &pr); err != nil {
		return errorEnvelope("invalid_request", "bad scheduler payload: "+err.Error())
	}
	p := state.Load()
	if p == nil {
		return errorEnvelope("policy_unavailable", "key-account-bind: binding policy is unavailable")
	}

	scope, authenticated := authenticatedCallerScope(pr.Options)
	if !authenticated {
		return errorEnvelope("caller_identity_missing", "key-account-bind: authenticated caller_scope is required")
	}

	b := p.bindingForScope(scope)
	if pr.InspectPool {
		if b == nil {
			return errorEnvelope("key_not_bound", "account usage requires an explicit key binding")
		}
		return okEnvelope(schedulerPickResponse{Handled: true, AuthorizedAuthIDs: b.candidateIDs(pr.PoolCandidates)})
	}
	if b == nil {
		if p.unboundPassthrough {
			return okEnvelope(schedulerPickResponse{Handled: false})
		}
		return errorEnvelope("key_not_bound",
			"key-account-bind: downstream key is not bound to any credential group (unbound=deny)")
	}

	allowed := make([]schedulerAuthCandidate, 0, len(pr.Candidates))
	for _, cand := range pr.Candidates {
		if !b.matches(cand.ID) {
			continue
		}
		if !candidateUsable(cand.Status) {
			continue
		}
		allowed = append(allowed, cand)
	}
	if len(allowed) == 0 {
		pool := b.candidateIDs(pr.PoolCandidates)
		if len(pool) > 0 {
			return okEnvelope(schedulerPickResponse{Handled: true, Reject: true,
				UnavailableAuthIDs: pool, RejectCode: "bound_pool_unavailable",
				RejectReason: "No available credential in this key's assigned pool"})
		}
		// Isolation guard: DO NOT return Handled=false here — that would let
		// the host schedule any candidate and silently break isolation.
		return errorEnvelope("auth_not_bound",
			fmt.Sprintf("key-account-bind: no eligible credential for this key among %d candidate(s)", len(pr.Candidates)))
	}

	picked, ok := selectAllowed(p.strategy, selectionScope(scope, pr), allowed, requestProviders(pr))
	if !ok {
		return errorEnvelope("auth_unavailable",
			fmt.Sprintf("key-account-bind: no eligible credential for strategy %s", p.strategy))
	}
	return okEnvelope(schedulerPickResponse{AuthID: picked.ID, Handled: true})
}

// candidateIDs shares the exact binding rules between diagnostics and inventory.
func (b *binding) candidateIDs(candidates []schedulerAuthCandidate) []string {
	ids := make([]string, 0)
	for _, candidate := range candidates {
		if b.matches(candidate.ID) {
			ids = append(ids, candidate.ID)
		}
	}
	return ids
}
