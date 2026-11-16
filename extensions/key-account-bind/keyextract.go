package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func callerScopeForKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + key))
	return hex.EncodeToString(sum[:])
}

func authenticatedCallerScope(options schedulerPickOptions) (string, bool) {
	scope, ok := options.Metadata["caller_scope"].(string)
	if !ok || len(scope) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(scope); err != nil {
		return "", false
	}
	return strings.ToLower(scope), true
}

// candidateUsable rejects explicit hard-disabled states. The host owns
// request-specific eligibility; a historical error status must not prevent
// recovery after its cooldown expires.
func candidateUsable(status string) bool {
	s := normalizeStatus(status)
	switch s {
	case "disabled", "expired", "revoked", "invalid", "unavailable",
		"cooldown", "cooling_down", "quota_exhausted", "exhausted", "blocked":
		return false
	default:
		return true
	}
}

func normalizeStatus(status string) string {
	s := strings.ToLower(strings.TrimSpace(status))
	s = strings.NewReplacer("-", "_", " ", "_").Replace(s)
	return s
}
