package antigravity

import (
	"fmt"
	"os"
	"strings"
)

// ClientCredentials resolves the OAuth client shared by login and token refresh.
// Credentials are supplied by the operator and must never appear in errors.
func ClientCredentials() (clientID, clientSecret string, err error) {
	clientID = strings.TrimSpace(os.Getenv("ANTIGRAVITY_OAUTH_CLIENT_ID"))
	clientSecret = strings.TrimSpace(os.Getenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET"))
	if clientID == "" || clientSecret == "" {
		return "", "", fmt.Errorf("antigravity OAuth requires ANTIGRAVITY_OAUTH_CLIENT_ID and ANTIGRAVITY_OAUTH_CLIENT_SECRET")
	}
	return clientID, clientSecret, nil
}
