package antigravity

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestClientCredentialsRequiresBothValues(t *testing.T) {
	for _, tc := range []struct {
		name, id, secret string
	}{
		{"both missing", "", ""},
		{"ID missing", " ", "synthetic-secret"},
		{"secret missing", "synthetic-id", "\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", tc.id)
			t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", tc.secret)
			id, secret, err := ClientCredentials()
			if err == nil || id != "" || secret != "" {
				t.Fatal("missing configuration must return an error without partial credentials")
			}
			if strings.Contains(err.Error(), "synthetic-") || !strings.Contains(err.Error(), "ANTIGRAVITY_OAUTH_CLIENT_SECRET") {
				t.Fatalf("expected actionable configuration error without values: %v", err)
			}
			auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("unconfigured OAuth must not issue a network request")
				return nil, nil
			})})
			if got := auth.BuildAuthURL("state", "http://localhost/callback"); got != "" {
				t.Fatal("unconfigured OAuth must not produce an authorization URL")
			}
			if _, errExchange := auth.ExchangeCodeForTokens(context.Background(), "code", "http://localhost/callback"); errExchange == nil {
				t.Fatal("unconfigured exchange must fail before network access")
			}
		})
	}
}

func TestClientCredentialsUsedByAuthorizationAndExchange(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", " test-client-id \n")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "\ttest-client-secret ")
	id, secret, err := ClientCredentials()
	if err != nil || id != "test-client-id" || secret != "test-client-secret" {
		t.Fatal("credentials must be trimmed")
	}
	const redirect = "http://localhost/callback"
	calls := 0
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != TokenEndpoint || req.Method != http.MethodPost {
			t.Fatalf("unexpected token request: %s %s", req.Method, req.URL)
		}
		if errParse := req.ParseForm(); errParse != nil {
			t.Fatal(errParse)
		}
		for key, want := range map[string]string{
			"client_id": id, "client_secret": secret, "code": "test-code",
			"grant_type": "authorization_code", "redirect_uri": redirect,
		} {
			if req.PostForm.Get(key) != want {
				t.Errorf("unexpected token form field %s", key)
			}
		}
		return jsonResponse(`{"access_token":"test-access","expires_in":3600}`), nil
	})})
	parsed, err := url.Parse(auth.BuildAuthURL("state", redirect))
	if err != nil || parsed.Query().Get("client_id") != id || parsed.Query().Has("client_secret") {
		t.Fatal("authorization URL must use the configured ID without the secret")
	}
	token, err := auth.ExchangeCodeForTokens(context.Background(), "test-code", redirect)
	if err != nil || token == nil || token.AccessToken != "test-access" || calls != 1 {
		t.Fatalf("configured token exchange failed: %v (calls=%d)", err, calls)
	}
}
