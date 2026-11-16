package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type scopedAffinityScheduler struct {
	mu                  sync.Mutex
	allowed             map[string][]string
	calls               int
	failInspection      bool
	unhandledInspection bool
	declinePick         bool
	unsafeDelegate      bool
}

func (s *scopedAffinityScheduler) InspectAuthPool(_ context.Context, req pluginapi.SchedulerPickRequest) ([]string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failInspection {
		return nil, true, fmt.Errorf("inspection failed")
	}
	if s.unhandledInspection {
		return nil, false, nil
	}
	scope, _ := req.Options.Metadata[cliproxyexecutor.CallerScopeMetadataKey].(string)
	return append([]string(nil), s.allowed[scope]...), true, nil
}
func (s *scopedAffinityScheduler) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.declinePick {
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	if s.unsafeDelegate {
		return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin}, true, nil
	}
	if len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}, true, fmt.Errorf("empty candidates")
	}
	candidate := req.Candidates[s.calls%len(req.Candidates)]
	s.calls++
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: candidate.ID}, true, nil
}

func scopedAffinitySetup(t *testing.T) (*Manager, *SessionAffinitySelector, *scopedAffinityScheduler, []*Auth) {
	t.Helper()
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{RebindOn: "quota", TTL: time.Hour})
	t.Cleanup(affinity.Stop)
	manager := NewManager(nil, affinity, nil)
	accounts := []*Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}, {ID: "c", Provider: "codex"}}
	scheduler := &scopedAffinityScheduler{allowed: map[string][]string{"tenant-a": {"a", "b"}, "tenant-b": {"c"}}}
	return manager, affinity, scheduler, accounts
}
func scopedOpts(scope, session string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {session}}, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: scope}}
}
func scopedPick(t *testing.T, m *Manager, s PluginScheduler, opts cliproxyexecutor.Options, candidates, pool []*Auth) (*Auth, error) {
	t.Helper()
	selected, handled, err := m.pickViaPluginScheduler(context.Background(), s, "codex", []string{"codex"}, "model", opts, nil, candidates, pool)
	if !handled {
		t.Fatal("isolated request fell through to unrestricted selector")
	}
	return selected, err
}
func TestScopedPluginAffinityStickyAndCallerIsolation(t *testing.T) {
	m, _, s, pool := scopedAffinitySetup(t)
	for _, scope := range []string{"tenant-a", "tenant-b", "tenant-a"} {
		var first string
		for i := 0; i < 8; i++ {
			a, err := scopedPick(t, m, s, scopedOpts(scope, "same-session-id"), pool, pool)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				first = a.ID
			} else if a.ID != first {
				t.Fatalf("session rotated: %s -> %s", first, a.ID)
			}
			if scope == "tenant-b" && a.ID != "c" || scope == "tenant-a" && a.ID == "c" {
				t.Fatal("cross-tenant selection")
			}
		}
	}
	if s.calls != 2 {
		t.Fatalf("fallback calls=%d, want two independent caller bindings", s.calls)
	}
}
func TestScopedPluginAffinityFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing-scope", "inspection-error", "unhandled-inspection", "unknown-auth", "empty-pool", "declined-pick", "unsafe-delegate"} {
		t.Run(mode, func(t *testing.T) {
			m, _, s, pool := scopedAffinitySetup(t)
			opts := scopedOpts("tenant-a", "s")
			switch mode {
			case "missing-scope":
				delete(opts.Metadata, cliproxyexecutor.CallerScopeMetadataKey)
			case "inspection-error":
				s.failInspection = true
			case "unhandled-inspection":
				s.unhandledInspection = true
			case "unknown-auth":
				s.allowed["tenant-a"] = []string{"does-not-exist"}
			case "empty-pool":
				s.allowed["tenant-a"] = nil
			case "declined-pick":
				s.declinePick = true
			case "unsafe-delegate":
				s.unsafeDelegate = true
			}
			a, err := scopedPick(t, m, s, opts, pool, pool)
			if err == nil || a != nil {
				t.Fatalf("unsafe selection: auth=%v error=%v", a, err)
			}
		})
	}
}
func TestScopedPluginAffinityRevocation(t *testing.T) {
	m, _, s, pool := scopedAffinitySetup(t)
	first, err := scopedPick(t, m, s, scopedOpts("tenant-a", "s"), pool, pool)
	if err != nil {
		t.Fatal(err)
	}
	remaining := "a"
	if first.ID == remaining {
		remaining = "b"
	}
	s.allowed["tenant-a"] = []string{remaining}
	next, err := scopedPick(t, m, s, scopedOpts("tenant-a", "s"), pool, pool)
	if err != nil || next.ID != remaining {
		t.Fatalf("revoked auth retained: %v %v", next, err)
	}
}
func TestScopedPluginAffinityConcurrentFirstSelection(t *testing.T) {
	m, affinity, s, pool := scopedAffinitySetup(t)
	const n = 24
	var wg sync.WaitGroup
	ids := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opts := scopedOpts("tenant-a", "new-session")
			a, _, err := m.pickViaPluginScheduler(context.Background(), s, "codex", []string{"codex"}, "model", opts, nil, pool, pool)
			if err != nil {
				ids <- "error"
			} else {
				ids <- a.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	var first string
	for id := range ids {
		if id == "error" {
			t.Fatal("concurrent selection failed")
		}
		if first == "" {
			first = id
		} else if first != id {
			t.Fatal("first requests raced across accounts")
		}
	}
	if s.calls != 1 {
		t.Fatalf("fallback calls=%d", s.calls)
	}
	affinity.scopedGate.mu.Lock()
	defer affinity.scopedGate.mu.Unlock()
	if len(affinity.scopedGate.entries) != 0 {
		t.Fatal("selection gates leaked")
	}
}
func TestScopedPluginAffinityTransientPreservesBinding(t *testing.T) {
	m, affinity, s, pool := scopedAffinitySetup(t)
	first, err := scopedPick(t, m, s, scopedOpts("tenant-a", "s"), pool, pool)
	if err != nil {
		t.Fatal(err)
	}
	unavailable := first.Clone()
	unavailable.Unavailable = true
	unavailable.NextRetryAfter = time.Now().Add(time.Minute)
	unavailable.LastError = &Error{HTTPStatus: 503, Message: "temporary upstream failure"}
	authorized := []*Auth{pool[0], pool[1], pool[2]}
	for i, a := range authorized {
		if a.ID == first.ID {
			authorized[i] = unavailable
		}
	}
	remaining := []*Auth{}
	for _, a := range pool {
		if a.ID != first.ID {
			remaining = append(remaining, a)
		}
	}
	failedOver, err := scopedPick(t, m, s, scopedOpts("tenant-a", "s"), remaining, authorized)
	if err != nil || failedOver.ID == first.ID {
		t.Fatalf("transient failover: %v %v", failedOver, err)
	}
	affinity.OnResult(Result{AuthID: failedOver.ID, Provider: "codex", Model: "model", Success: true, Options: scopedOpts("tenant-a", "s")})
	recovered, err := scopedPick(t, m, s, scopedOpts("tenant-a", "s"), pool, pool)
	if err != nil || recovered.ID != first.ID {
		t.Fatalf("temporary failover stole binding: %v %v", recovered, err)
	}
}
func TestScopedPluginAffinityQuotaRebind(t *testing.T) {
	m, affinity, s, pool := scopedAffinitySetup(t)
	opts := scopedOpts("tenant-a", "s")
	first, err := scopedPick(t, m, s, opts, pool, pool)
	if err != nil {
		t.Fatal(err)
	}
	affinity.OnResult(Result{AuthID: first.ID, Provider: "codex", Model: "model", Error: &Error{HTTPStatus: 429, Message: "quota exceeded"}, Options: opts})
	remaining := []*Auth{}
	for _, a := range pool {
		if a.ID != first.ID {
			remaining = append(remaining, a)
		}
	}
	next, err := scopedPick(t, m, s, scopedOpts("tenant-a", "s"), remaining, pool)
	if err != nil || next.ID == first.ID {
		t.Fatalf("quota did not switch: %v %v", next, err)
	}
	recovered, err := scopedPick(t, m, s, scopedOpts("tenant-a", "s"), pool, pool)
	if err != nil || recovered.ID != next.ID {
		t.Fatalf("quota binding did not persist: %v %v", recovered, err)
	}
}
