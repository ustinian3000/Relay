package auth

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQuotaDescriptionsDistinguishWindowsAndPreserveProtocol(t *testing.T) {
	for _, test := range []struct{ name, status5, status7, used5, used7, want string }{
		{"five hour", "rejected", "allowed", "1.0", "0.43", "五小时额度已用尽"},
		{"weekly", "allowed", "rejected", "0.23", "1.0", "周额度已用尽"},
		{"both", "rejected", "rejected", "1.0", "1.0", "周额度已用尽"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			account := &Auth{ID: "private-account-id", Provider: "claude", Quota: QuotaState{Signals: map[string]string{
				"anthropic-ratelimit-unified-5h-status":      test.status5,
				"anthropic-ratelimit-unified-7d-status":      test.status7,
				"anthropic-ratelimit-unified-5h-utilization": test.used5,
				"anthropic-ratelimit-unified-7d-utilization": test.used7,
				"anthropic-ratelimit-unified-5h-reset":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
				"anthropic-ratelimit-unified-7d-reset":       strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10),
			}}}
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			cause := newModelCooldownErrorWithCause("model", "claude", time.Hour, &Error{Code: "rate_limit_error", Message: "quota exhausted", HTTPStatus: 429})
			got := manager.describePoolQuota(context.Background(), cause, []*Auth{account})
			for _, text := range []string{test.want, "预计还需等待", "恢复时间", "日本时间", "quota_windows"} {
				if !strings.Contains(got.Error(), text) {
					t.Fatalf("missing %q: %s", text, got)
				}
			}
			if test.name == "five hour" && !strings.Contains(got.Error(), "周额度已使用43%，未耗尽") {
				t.Fatal(got)
			}
			if strings.Contains(got.Error(), account.ID) {
				t.Fatal("private auth ID leaked")
			}
			if statusCodeFromError(got) != 429 || SafeResponseHeaders(got).Get("Retry-After") != "3600" {
				t.Fatalf("protocol changed: %v", got)
			}
		})
	}
}

func TestQuotaUsageParsingHandlesMissingAndExpiredWindows(t *testing.T) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"five_hour":{"utilization":100,"resets_at":"2026-11-09T12:00:00Z"},"seven_day":{"utilization":43,"resets_at":"2026-11-15T00:00:00Z"}}`)
	windows := parseClaudeQuotaUsage(body, now)
	if len(windows) != 2 || !windows[0].Exhausted || windows[1].Exhausted {
		t.Fatalf("wrong usage: %+v", windows)
	}
	for _, body := range []string{`{"five_hour":null}`, `{"five_hour":{"utilization":101}}`, `{"five_hour":{"utilization":100,"resets_at":"2026-11-08T00:00:00Z"}}`, `not json`} {
		if got := parseClaudeQuotaUsage([]byte(body), now); len(got) != 0 {
			t.Fatalf("invalid usage accepted: %s %+v", body, got)
		}
	}
	windows = parseClaudeQuotaUsage([]byte(`{"five_hour":{"utilization":100}}`), now)
	if len(windows) != 1 || windows[0].ResetsAt != nil || !windows[0].Exhausted {
		t.Fatalf("unknown reset mishandled: %+v", windows)
	}
}

func TestQuotaDescriptionCacheDeduplicatesAndExpires(t *testing.T) {
	entry := &quotaDescriptionCacheEntry{}
	now := time.Now()
	var calls atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			entry.load(now, func() quotaLookupResult { calls.Add(1); return quotaLookupResult{} })
		}()
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate failed queries: %d", calls.Load())
	}
	entry.load(now.Add(time.Minute), func() quotaLookupResult { calls.Add(1); return quotaLookupResult{} })
	if calls.Load() != 2 {
		t.Fatal("failed-query cache never expired")
	}
}

type quotaDescriptionTransport func(*http.Request) (*http.Response, error)

func (fn quotaDescriptionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestQuotaUsageQueryIsReadOnlyAndSanitized(t *testing.T) {
	client := &http.Client{Transport: quotaDescriptionTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.String() != "https://api.anthropic.com/api/oauth/usage" || r.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Fatal("incorrect quota request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":100},"seven_day":{"utilization":43}}`)), Header: make(http.Header)}, nil
	})}
	if windows := fetchAccountQuota(context.Background(), client, &Auth{Provider: "claude", Metadata: map[string]any{"access_token": "private-test-token"}}, time.Now()).windows; len(windows) != 2 {
		t.Fatalf("usage query failed: %+v", windows)
	}
}

func TestQuotaDescriptionUnknownDoesNotInventWindow(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	cause := newModelCooldownError("model", "claude", time.Hour)
	got := manager.describePoolQuota(context.Background(), cause, []*Auth{{ID: "a", Provider: "claude"}})
	if !strings.Contains(got.Error(), "额度类型未知") || strings.Contains(got.Error(), "五小时额度已用尽") {
		t.Fatal(got)
	}
	nonquota := &Error{Code: "proxy_error", Message: "proxy forbidden", HTTPStatus: 403}
	if manager.describePoolQuota(context.Background(), nonquota, nil) != nonquota {
		t.Fatal("non-quota error relabelled")
	}
}
