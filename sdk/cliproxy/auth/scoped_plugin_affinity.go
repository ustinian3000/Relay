package auth

import (
	"context"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type scopedPluginFallback struct {
	pick func(context.Context, string, string, cliproxyexecutor.Options, []*Auth) (*Auth, error)
}

func (s scopedPluginFallback) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, candidates []*Auth) (*Auth, error) {
	return s.pick(ctx, provider, model, opts, candidates)
}

func scopedAffinityError(message string) error {
	return wrapRequestStopError(&Error{Code: "scoped_affinity_unavailable", Message: message, HTTPStatus: 503})
}

func (m *Manager) pickViaScopedPluginAffinity(ctx context.Context, affinity *SessionAffinitySelector, scheduler PluginScheduler, provider string, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, candidates, pool []*Auth) (*Auth, bool, error) {
	opts.EnsureMetadata()
	clearSchedulerSelectionID(opts.Metadata)
	scope := sessionMetadataString(opts.Metadata, cliproxyexecutor.CallerScopeMetadataKey)
	if scope == "" {
		return nil, true, scopedAffinityError("Authenticated caller scope is required for isolated session affinity")
	}
	if pool == nil {
		pool = m.snapshotAuths()
	}
	authorized, err := inspectAuthorizedAuthPool(ctx, scheduler, pluginapi.SchedulerPickRequest{
		Provider: provider, Providers: providers, Model: model, Stream: opts.Stream, Options: schedulerOptions(opts),
	}, pool)
	if err != nil {
		return nil, true, scopedAffinityError(err.Error())
	}
	allowed := make(map[string]bool, len(authorized))
	for _, auth := range authorized {
		allowed[auth.ID] = true
	}
	scoped := make([]*Auth, 0, len(candidates))
	for _, auth := range candidates {
		if auth != nil && allowed[auth.ID] {
			scoped = append(scoped, auth)
		}
	}
	if len(scoped) == 0 {
		ids := make([]string, 0, len(authorized))
		for _, auth := range authorized {
			ids = append(ids, auth.ID)
		}
		return nil, true, m.schedulerPoolAvailabilityError(ctx, provider, model, opts, authorized, ids)
	}
	fallback := scopedPluginFallback{pick: func(pickCtx context.Context, pickProvider, pickModel string, pickOpts cliproxyexecutor.Options, available []*Auth) (*Auth, error) {
		selected, handled, pickErr := m.pickViaPluginDecision(pickCtx, scheduler, pickProvider, providers, pickModel, pickOpts, tried, available, authorized)
		if pickErr != nil {
			return nil, pickErr
		}
		if !handled || selected == nil || !allowed[selected.ID] || pickSchedulerAuthByID(available, selected.ID) == nil {
			return nil, scopedAffinityError("Scheduler did not select a credential inside the authorized session pool")
		}
		return selected, nil
	}}
	selectorCtx := selectorContextForAvailableAuths(ctx, affinity, model)
	transient := m.transientlyUnavailableSessionAffinityAuthIDs(selectorCtx, affinity, authorized, model, time.Now(), opts.Headers)
	selectorCtx = withSessionAffinityTransientAuthIDs(selectorCtx, transient)
	selected, pickErr := affinity.pickWithFallback(selectorCtx, provider, model, opts, scoped, fallback)
	if pickErr == nil && (selected == nil || !allowed[selected.ID] || pickSchedulerAuthByID(scoped, selected.ID) == nil) {
		return nil, true, scopedAffinityError("Session selection escaped the authorized credential pool")
	}
	return selected, true, pickErr
}

// scopedAffinityGate serializes selection, not inference, within one tenant route.
// References include waiters, so completed routes do not accumulate lock entries.
type scopedAffinityGate struct {
	mu      sync.Mutex
	entries map[string]*scopedAffinityGateEntry
}
type scopedAffinityGateEntry struct {
	mu   sync.Mutex
	refs int
}

func (g *scopedAffinityGate) lock(key string) func() {
	g.mu.Lock()
	if g.entries == nil {
		g.entries = make(map[string]*scopedAffinityGateEntry)
	}
	entry := g.entries[key]
	if entry == nil {
		entry = &scopedAffinityGateEntry{}
		g.entries[key] = entry
	}
	entry.refs++
	g.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		g.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(g.entries, key)
		}
		g.mu.Unlock()
	}
}

func (s *SessionAffinitySelector) pickWithFallback(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, candidates []*Auth, fallback Selector) (*Auth, error) {
	if s.scopedGate != nil {
		scope := sessionMetadataString(opts.Metadata, cliproxyexecutor.CallerScopeMetadataKey)
		unlock := s.scopedGate.lock(scope + "::" + provider + "::" + canonicalModelKey(model))
		defer unlock()
	}
	view := *s
	view.fallbackOverride = fallback
	return view.Pick(ctx, provider, model, opts, candidates)
}

func (s *SessionAffinitySelector) pickFallback(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, candidates []*Auth) (*Auth, error) {
	fallback := s.fallback
	if s.fallbackOverride != nil {
		fallback = s.fallbackOverride
	}
	return fallback.Pick(ctx, provider, model, opts, candidates)
}

func scopedAffinitySessionID(sessionID string, metadata map[string]any) string {
	if scope := sessionMetadataString(metadata, cliproxyexecutor.CallerScopeMetadataKey); scope != "" {
		return scope + "::" + sessionID
	}
	return sessionID
}
func affinityCacheKey(provider, sessionID, model string, metadata map[string]any) string {
	return provider + "::" + scopedAffinitySessionID(sessionID, metadata) + "::" + model
}
