package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

type frontendAuthRequest struct {
	Headers http.Header
	Query   url.Values
}

type frontendAuthResponse struct {
	Authenticated  bool
	Principal      string
	DenyStatusCode int
	DenyMessage    string
}

func handleAuthenticate(raw []byte) []byte {
	var request frontendAuthRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return okEnvelope(frontendAuthResponse{DenyStatusCode: http.StatusBadRequest, DenyMessage: "invalid authentication request"})
	}
	policy := state.Load()
	if policy == nil {
		return okEnvelope(frontendAuthResponse{DenyStatusCode: http.StatusServiceUnavailable, DenyMessage: "account binding policy unavailable"})
	}
	authorization := request.Headers.Get("Authorization")
	parts := strings.SplitN(authorization, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		authorization = strings.TrimSpace(parts[1])
	}
	candidates := []string{
		authorization,
		request.Headers.Get("X-Goog-Api-Key"),
		request.Headers.Get("X-Api-Key"),
		request.Query.Get("key"),
		request.Query.Get("auth_token"),
	}
	for _, key := range candidates {
		if key != "" && policy.bindingForScope(callerScopeForKey(key)) != nil {
			return okEnvelope(frontendAuthResponse{Authenticated: true, Principal: key})
		}
	}
	return okEnvelope(frontendAuthResponse{DenyStatusCode: http.StatusUnauthorized, DenyMessage: "missing or unbound API key"})
}
