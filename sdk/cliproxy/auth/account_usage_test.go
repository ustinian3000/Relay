package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type usageTestInspector struct {
	fakePluginScheduler
	allowed         map[string][]string
	inspectionError bool
}

func (s *usageTestInspector) InspectAuthPool(_ context.Context, req pluginapi.SchedulerPickRequest) ([]string, bool, error) {
	if s.inspectionError {
		return nil, true, fmt.Errorf("private plugin failure")
	}
	if !req.InspectPool || len(req.Candidates) != 0 {
		return nil, true, fmt.Errorf("not read-only")
	}
	for _, candidate := range req.PoolCandidates {
		if len(candidate.Metadata) != 0 {
			return nil, true, fmt.Errorf("credential metadata leaked to inspector")
		}
	}
	scope, _ := req.Options.Metadata[pluginapi.CallerScopeMetadataKey].(string)
	return s.allowed[scope], true, nil
}

type usageTestTransports struct {
	transport http.RoundTripper
	account   func(*Auth)
}

func (p usageTestTransports) RoundTripperFor(account *Auth) http.RoundTripper {
	if p.account != nil {
		p.account(account)
	}
	return p.transport
}

func usageTestManager(t *testing.T, accounts ...*Auth) *Manager {
	t.Helper()
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	for _, account := range accounts {
		if _, err := m.Register(context.Background(), account); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func TestAccountUsageScopesLiveQueriesAndPreservesState(t *testing.T) {
	secret := t.Name()
	claude := &Auth{ID: "claude-private@example.test.json", Provider: "claude", ProxyURL: "http://user:proxy-secret@proxy.example:8080", Metadata: map[string]any{"access_token": secret + "-a"}}
	codex := &Auth{ID: "codex-other@example.test.json", Provider: "codex", Metadata: map[string]any{"access_token": secret + "-b", "account_id": "private-account-id"}}
	m := usageTestManager(t, claude, codex)
	m.SetPluginScheduler(&usageTestInspector{allowed: map[string][]string{"scope-a": {claude.ID}, "scope-b": {codex.ID}}})
	var claudeCalls, codexCalls atomic.Int32
	m.SetRoundTripperProvider(usageTestTransports{transport: quotaDescriptionTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Error("quota lookup used a mutating method")
		}
		body := `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":25,"reset_after_seconds":3600},"secondary_window":{"limit_window_seconds":604800,"used_percent":12,"reset_after_seconds":172800}}}`
		if r.URL.Host == "api.anthropic.com" {
			claudeCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+secret+"-a" {
				t.Error("wrong Claude credential")
			}
			doc := ojson.NewDocString(`{"five_hour":{"utilization":100},"seven_day":{"utilization":43}}`)
			_ = doc.Set("five_hour.resets_at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			_ = doc.Set("seven_day.resets_at", time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339))
			body = doc.String()
		} else {
			codexCalls.Add(1)
			if r.URL.Host != "chatgpt.com" || r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("Chatgpt-Account-Id") != "private-account-id" {
				t.Error("wrong Codex quota route")
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})})
	before, _ := m.GetByID(claude.ID)
	first, err := m.AccountUsage(context.Background(), "scope-a")
	if err != nil || len(first.Accounts) != 1 || claudeCalls.Load() != 1 || codexCalls.Load() != 0 {
		t.Fatalf("scope A leaked: %+v %v", first, err)
	}
	row := first.Accounts[0]
	if row.Status != "quota_exhausted" || row.FiveHour == nil || *row.FiveHour.UsedPercent != 100 || row.Weekly == nil || *row.Weekly.UsedPercent != 43 || row.CooldownSeconds == nil {
		t.Fatalf("wrong quota: %+v", row)
	}
	second, err := m.AccountUsage(context.Background(), "scope-a")
	if err != nil || second.Accounts[0].ID != row.ID || second.Accounts[0].Source != "cache" || claudeCalls.Load() != 1 {
		t.Fatal("cache or stable identity failed")
	}
	other, err := m.AccountUsage(context.Background(), "scope-b")
	if err != nil || len(other.Accounts) != 1 || codexCalls.Load() != 1 || *other.Accounts[0].FiveHour.UsedPercent != 25 {
		t.Fatalf("Codex quota failed: %+v %v", other, err)
	}
	doc := ojson.NewDocString(`{}`)
	_ = doc.Set("accounts", append(first.Accounts, other.Accounts...))
	for _, sensitive := range []string{claude.ID, codex.ID, secret, "private-account-id", "proxy-secret", "proxy.example"} {
		if strings.Contains(doc.String(), sensitive) {
			t.Fatalf("private field leaked: %s", sensitive)
		}
	}
	after, _ := m.GetByID(claude.ID)
	if before.Generation != after.Generation || before.Quota.Exceeded != after.Quota.Exceeded || before.NextRetryAfter != after.NextRetryAfter {
		t.Fatal("query changed routing state")
	}
}

func TestAccountUsageReportsPartialFailureWithoutGuessingZero(t *testing.T) {
	a := &Auth{ID: "valid", Provider: "codex", Metadata: map[string]any{"access_token": t.Name() + "-a", "account_id": "a"}}
	b := &Auth{ID: "invalid", Provider: "claude", Metadata: map[string]any{"access_token": t.Name() + "-b"}}
	m := usageTestManager(t, a, b)
	m.SetPluginScheduler(&usageTestInspector{allowed: map[string][]string{"scope": {a.ID, b.ID}}})
	m.SetRoundTripperProvider(usageTestTransports{transport: quotaDescriptionTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.anthropic.com" {
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("private token and proxy details")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":0}}}`)), Header: make(http.Header)}, nil
	})})
	result, err := m.AccountUsage(context.Background(), "scope")
	if err != nil || len(result.Accounts) != 2 {
		t.Fatal(result, err)
	}
	for _, row := range result.Accounts {
		if row.Provider == "claude" {
			if row.Status != "authentication_failed" || row.FiveHour != nil || row.Weekly != nil || row.Error == nil || row.Error.Code != "upstream_unauthorized" {
				t.Fatalf("missing failure: %+v", row)
			}
		} else if row.Status != "ok" || row.FiveHour == nil || *row.FiveHour.UsedPercent != 0 || row.Weekly != nil {
			t.Fatalf("unknown was replaced with zero: %+v", row)
		} else if row.Available != nil {
			t.Fatal("partial quota information was labelled fully available")
		}
	}
}

func TestAccountUsageFailsClosedBeforeNetwork(t *testing.T) {
	m := usageTestManager(t, &Auth{ID: "account", Provider: "claude", Metadata: map[string]any{"access_token": t.Name()}})
	var calls atomic.Int32
	m.SetRoundTripperProvider(usageTestTransports{transport: quotaDescriptionTransport(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, fmt.Errorf("must not query") })})
	if _, err := m.AccountUsage(context.Background(), "scope"); err == nil {
		t.Fatal("missing plugin allowed inventory")
	}
	m.SetPluginScheduler(&usageTestInspector{allowed: map[string][]string{"scope": {"outside"}}})
	if _, err := m.AccountUsage(context.Background(), "scope"); err == nil {
		t.Fatal("unknown account accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("network contacted before authorization")
	}
}

func TestCodexQuotaWindowsUseDurationNotSlotPosition(t *testing.T) {
	result := parseCodexQuotaUsage([]byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":100,"reset_after_seconds":86400},"secondary_window":{"limit_window_seconds":18000,"used_percent":33,"reset_after_seconds":600}}}`), time.Now())
	if len(result) != 2 || result[0].Kind != "seven_day" || result[1].Kind != "five_hour" {
		t.Fatalf("wrong windows: %+v", result)
	}
	custom := parseCodexQuotaUsage([]byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":3600,"used_percent":10}}}`), time.Now())
	if len(custom) != 1 || custom[0].Kind != "custom" || custom[0].WindowSeconds != 3600 {
		t.Fatal("nonstandard period was mislabeled")
	}
	for _, body := range []string{`{"rate_limit":{"primary_window":{"limit_window_seconds":0,"used_percent":10}}}`, `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":null}}}`} {
		if windows := parseCodexQuotaUsage([]byte(body), time.Now()); len(windows) > 0 {
			t.Fatal("unknown quota window was guessed")
		}
	}
}

func TestAccountUsageCodexMonthlyWindowIsNotAWeeklyOrFiveHourQuota(t *testing.T) {
	for _, used := range []int{2, 100} {
		t.Run(fmt.Sprint(used), func(t *testing.T) {
			a := &Auth{ID: "monthly", Provider: "codex", Metadata: map[string]any{"access_token": t.Name(), "account_id": "monthly-account"}}
			m := usageTestManager(t, a)
			m.SetPluginScheduler(&usageTestInspector{allowed: map[string][]string{"scope": {a.ID}}})
			m.SetRoundTripperProvider(usageTestTransports{transport: quotaDescriptionTransport(func(*http.Request) (*http.Response, error) {
				doc := ojson.NewDocString(`{"rate_limit":{"primary_window":{"limit_window_seconds":2592000,"reset_after_seconds":2149473}}}`)
				_ = doc.Set("rate_limit.primary_window.used_percent", used)
				_ = doc.Set("rate_limit.allowed", used < 100)
				_ = doc.Set("rate_limit.limit_reached", used >= 100)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(doc.String())), Header: make(http.Header)}, nil
			})})
			result, err := m.AccountUsage(context.Background(), "scope")
			if err != nil {
				t.Fatal(err)
			}
			row := result.Accounts[0]
			if row.Monthly == nil || *row.Monthly.UsedPercent != float64(used) || row.Monthly.ResetsAt == nil || *row.Monthly.ResetsInSeconds < 2149400 || row.Weekly != nil || row.FiveHour != nil || len(row.Windows) != 1 || row.Windows[0].DurationSeconds != 2592000 {
				t.Fatalf("wrong monthly quota: %+v", row)
			}
			if used == 100 {
				if row.Status != "quota_exhausted" || row.CooldownSeconds == nil || *row.CooldownSeconds < 2149400 {
					t.Fatal("monthly recovery was discarded")
				}
				description := m.describePoolQuota(context.Background(), newModelCooldownError("test", "codex", time.Hour), []*Auth{a}).Error()
				if !strings.Contains(description, "30天额度已用尽") || strings.Contains(description, "五小时额度已用尽") {
					t.Fatal("monthly limit was mislabeled")
				}
			} else if row.Status != "ok" || row.Available == nil || !*row.Available {
				t.Fatal("provider allowed monthly account reported unknown")
			}
		})
	}
}

func TestAccountUsageConcurrentReadsDeduplicateRequests(t *testing.T) {
	a := &Auth{ID: "one", Provider: "claude", Metadata: map[string]any{"access_token": t.Name()}}
	m := usageTestManager(t, a)
	m.SetPluginScheduler(&usageTestInspector{allowed: map[string][]string{"scope": {a.ID}}})
	var calls atomic.Int32
	m.SetRoundTripperProvider(usageTestTransports{transport: quotaDescriptionTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":10}}`)), Header: make(http.Header)}, nil
	})})
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := m.AccountUsage(context.Background(), "scope"); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate requests: %d", calls.Load())
	}
}

func TestAccountUsageUsesEffectiveProxyWithoutChangingCredential(t *testing.T) {
	globalProxy := "http://global:private-global@global.proxy:8080"
	localProxy := "http://own:private-own@own.proxy:8080"
	a := &Auth{ID: "global", Provider: "claude", Metadata: map[string]any{"access_token": t.Name() + "-a"}}
	b := &Auth{ID: "own", Provider: "claude", ProxyURL: localProxy, Metadata: map[string]any{"access_token": t.Name() + "-b"}}
	m := usageTestManager(t, a, b)
	m.SetConfig(&internalconfig.Config{SDKConfig: internalconfig.SDKConfig{ProxyURL: globalProxy}})
	m.SetPluginScheduler(&usageTestInspector{allowed: map[string][]string{"scope": {a.ID, b.ID}}})
	var calls atomic.Int32
	m.SetRoundTripperProvider(usageTestTransports{account: func(account *Auth) {
		want := globalProxy
		if account.ID == "own" {
			want = localProxy
		}
		if account.ProxyURL != want {
			t.Error("quota query did not preserve the effective proxy")
		}
		calls.Add(1)
	}, transport: quotaDescriptionTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":10}}`)), Header: make(http.Header)}, nil
	})})
	if _, err := m.AccountUsage(context.Background(), "scope"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("proxy transport was not consulted")
	}
	stored, _ := m.GetByID(a.ID)
	if stored.ProxyURL != "" {
		t.Fatal("global proxy was written into credential")
	}
}

func TestAccountUsageDisabledAndSignInRequiredAreNotQueried(t *testing.T) {
	disabled := &Auth{ID: "disabled", Provider: "claude", Disabled: true, Metadata: map[string]any{"access_token": t.Name() + "-a"}}
	invalid := &Auth{ID: "login", Provider: "claude", RequiresSignIn: true, Metadata: map[string]any{"access_token": t.Name() + "-b"}}
	m := usageTestManager(t, disabled, invalid)
	m.SetPluginScheduler(&usageTestInspector{allowed: map[string][]string{"scope": {disabled.ID, invalid.ID}}})
	var calls atomic.Int32
	m.SetRoundTripperProvider(usageTestTransports{transport: quotaDescriptionTransport(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, fmt.Errorf("not allowed") })})
	result, err := m.AccountUsage(context.Background(), "scope")
	if err != nil || len(result.Accounts) != 2 {
		t.Fatal(result, err)
	}
	states := map[string]bool{}
	for _, row := range result.Accounts {
		states[row.Status] = true
		if row.Available == nil || *row.Available || row.CooldownSeconds != nil {
			t.Fatalf("invalid inactive account state: %+v", row)
		}
	}
	if !states["disabled"] || !states["authentication_failed"] || calls.Load() != 0 {
		t.Fatal("disabled account was omitted or contacted")
	}
}

func TestAccountUsageInvalidProxyDoesNotFallBackDirect(t *testing.T) {
	a := &Auth{ID: "bad-proxy", Provider: "claude", ProxyURL: "bad-protocol://private-user:private-password@proxy:9999", Metadata: map[string]any{"access_token": t.Name()}}
	m := usageTestManager(t, a)
	m.SetPluginScheduler(&usageTestInspector{allowed: map[string][]string{"scope": {a.ID}}})
	result, err := m.AccountUsage(context.Background(), "scope")
	if err != nil {
		t.Fatal(err)
	}
	row := result.Accounts[0]
	if row.Status != "query_failed" || row.Error == nil || row.Error.Code != "invalid_proxy" || row.Available != nil || row.FiveHour != nil {
		t.Fatalf("unsafe proxy failure: %+v", row)
	}
}
