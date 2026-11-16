package management

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAntigravityManagementRefreshCredentials(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "test-client-id")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "test-client-secret")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		for key, want := range map[string]string{
			"client_id": "test-client-id", "client_secret": "test-client-secret",
			"grant_type": "refresh_token", "refresh_token": "test-refresh",
		} {
			if r.PostForm.Get(key) != want {
				t.Errorf("unexpected refresh field %s", key)
			}
		}
		_, _ = io.WriteString(w, `{"access_token":"new-access","expires_in":3600}`)
	}))
	defer server.Close()
	originalURL := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = server.URL
	t.Cleanup(func() { antigravityOAuthTokenURL = originalURL })
	h := &Handler{}
	makeAuth := func() *coreauth.Auth {
		return &coreauth.Auth{Metadata: map[string]any{"refresh_token": "test-refresh"}}
	}
	if token, err := h.refreshAntigravityOAuthAccessToken(context.Background(), makeAuth(), "direct"); err != nil || token != "new-access" || calls != 1 {
		t.Fatalf("configured refresh failed: %v (calls=%d)", err, calls)
	}
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	if _, err := h.refreshAntigravityOAuthAccessToken(context.Background(), makeAuth(), "direct"); err == nil || !strings.Contains(err.Error(), "ANTIGRAVITY_OAUTH_CLIENT_SECRET") || calls != 1 {
		t.Fatalf("missing configuration must fail before network access: %v (calls=%d)", err, calls)
	}
	validAuth := makeAuth()
	validAuth.Metadata["access_token"] = "existing-access"
	validAuth.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	if token, err := h.refreshAntigravityOAuthAccessToken(context.Background(), validAuth, "direct"); err != nil || token != "existing-access" {
		t.Fatalf("unexpired tokens must remain usable: %v", err)
	}
}

func TestAntigravityLoginRejectsMissingCredentials(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/management/antigravity-auth-url", nil)
	(&Handler{}).RequestAntigravityToken(c)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "ANTIGRAVITY_OAUTH_CLIENT_ID") || strings.Contains(w.Body.String(), `"state"`) {
		t.Fatalf("expected configuration error without a new OAuth session: %d %s", w.Code, w.Body.String())
	}
}
