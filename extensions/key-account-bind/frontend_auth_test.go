package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

func TestFrontendIdentifierIsAvailable(test *testing.T) {
	var result struct{ Result struct{ Identifier string } }
	if err := json.Unmarshal(dispatch("frontend_auth.identifier", nil), &result); err != nil || result.Result.Identifier != pluginName {
		test.Fatalf("frontend identifier unavailable: %+v", result)
	}
}

func TestFrontendOwnsBoundKeys(test *testing.T) {
	mustConfigure(test, "bindings: [\"sk-one=account-a\"]\n")
	for _, request := range []frontendAuthRequest{
		{Headers: http.Header{"Authorization": {"Bearer sk-one"}}},
		{Headers: http.Header{"X-Api-Key": {"sk-one"}}},
		{Query: url.Values{"key": {"sk-one"}}},
		{Query: url.Values{"auth_token": {"sk-one"}}},
	} {
		payload, _ := json.Marshal(request)
		var result struct{ Result frontendAuthResponse }
		if err := json.Unmarshal(handleAuthenticate(payload), &result); err != nil || !result.Result.Authenticated || result.Result.Principal != "sk-one" {
			test.Fatalf("bound key was not authenticated: %+v", result)
		}
	}
	for _, request := range []frontendAuthRequest{
		{},
		{Headers: http.Header{"Authorization": {"Bearer sk-unknown"}}},
		{Query: url.Values{"key": {"sk-unknown"}}},
	} {
		payload, _ := json.Marshal(request)
		var result struct{ Result frontendAuthResponse }
		if err := json.Unmarshal(handleAuthenticate(payload), &result); err != nil || result.Result.Authenticated || result.Result.DenyStatusCode != http.StatusUnauthorized {
			test.Fatalf("unbound key was not rejected: %+v", result)
		}
	}
}
