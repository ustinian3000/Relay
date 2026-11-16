package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type usageHTTPAuthentication struct{}

func (usageHTTPAuthentication) Identifier() string { return "usage-test" }
func (usageHTTPAuthentication) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if key != "key-a" && key != "key-b" {
		return nil, sdkaccess.NewInvalidCredentialError()
	}
	return &sdkaccess.Result{Provider: "usage-test", Principal: key}, nil
}

type usageHTTPInspector struct{ calls atomic.Int32 }

func (*usageHTTPInspector) PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	return pluginapi.SchedulerPickResponse{}, true, fmt.Errorf("inspection must not schedule")
}
func (s *usageHTTPInspector) InspectAuthPool(_ context.Context, req pluginapi.SchedulerPickRequest) ([]string, bool, error) {
	s.calls.Add(1)
	scope, _ := req.Options.Metadata[pluginapi.CallerScopeMetadataKey].(string)
	if scope == session.CallerScope("key-a") {
		return []string{"private-a@example.test.json"}, true, nil
	}
	if scope == session.CallerScope("key-b") {
		return []string{"private-b@example.test.json"}, true, nil
	}
	return nil, true, fmt.Errorf("unbound identity")
}

func TestAccountUsageHTTPUsesAuthenticatedPrincipalAndDoesNotLeak(t *testing.T) {
	server := newTestServer(t)
	server.accessManager.SetProviders([]sdkaccess.Provider{usageHTTPAuthentication{}})
	inspector := &usageHTTPInspector{}
	server.handlers.AuthManager.SetPluginScheduler(inspector)
	for index, name := range []string{"private-a@example.test.json", "private-b@example.test.json"} {
		_, err := server.handlers.AuthManager.Register(context.Background(), &coreauth.Auth{ID: name, Provider: "claude", Metadata: map[string]any{"refresh_token": "private-refresh-token"}, Quota: coreauth.QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": fmt.Sprint(0.11 + 0.22*float64(index))}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	var stableID string
	for _, test := range []struct {
		key  string
		want float64
	}{{"key-a", 11}, {"key-b", 33}, {"key-a", 11}} {
		req := httptest.NewRequest("GET", "/v1/account-usage?auth_id=private-b@example.test.json&caller_scope="+session.CallerScope("key-b"), nil)
		req.Header.Set("Authorization", "Bearer "+test.key)
		req.Header.Set("X-Caller-Scope", session.CallerScope("key-b"))
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		if recorder.Code != 200 {
			t.Fatalf("HTTP %d: %s", recorder.Code, recorder.Body)
		}
		body := recorder.Body.Bytes()
		if ojson.GetBytes(body, "accounts.#").Int() != 1 || ojson.GetBytes(body, "accounts.0.fiveHour.usedPercent").Float() != test.want {
			t.Fatalf("wrong pool response: %s", body)
		}
		for _, secret := range []string{"private-a", "private-b", "example.test", "private-refresh-token", "key-a", "key-b", "proxy_url"} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("private data leaked: %s", secret)
			}
		}
		if recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("public response can be cached")
		}
		if test.key == "key-a" {
			id := ojson.GetBytes(body, "accounts.0.id").String()
			if stableID != "" && id != stableID {
				t.Fatal("account ID changed between requests")
			}
			stableID = id
		}
	}
	before := inspector.calls.Load()
	for _, key := range []string{"", "invalid"} {
		req := httptest.NewRequest("GET", "/v1/account-usage", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		if recorder.Code != 401 {
			t.Fatalf("unauthenticated status: %d", recorder.Code)
		}
	}
	if inspector.calls.Load() != before {
		t.Fatal("unauthorized request inspected accounts")
	}
	server.handlers.AuthManager.SetPluginScheduler(nil)
	req := httptest.NewRequest("GET", "/v1/account-usage", nil)
	req.Header.Set("Authorization", "Bearer key-a")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)
	if recorder.Code != 503 || ojson.GetBytes(recorder.Body.Bytes(), "accounts").Exists() {
		t.Fatalf("missing inspector leaked inventory: %d %s", recorder.Code, recorder.Body)
	}
}
