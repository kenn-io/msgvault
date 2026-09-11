package carddav

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"path/filepath"

	"go.kenn.io/msgvault/internal/oauth"
)

// googleTokensDir separates CardDAV authorizations by configured OAuth app.
// Hashing the app name keeps arbitrary configuration keys out of path segments.
func googleTokensDir(tokensDir, app string) string {
	return filepath.Join(tokensDir, "carddav-google", fmt.Sprintf("%x", sha256.Sum256([]byte(app))))
}

// NewGoogleOAuthManager reuses a mail/calendar authorization only when its
// recorded client matches the selected app. An existing CardDAV authorization
// takes precedence so later mail setup cannot switch the connection's token.
func NewGoogleOAuthManager(secrets, tokensDir, app, email string, logger *slog.Logger) (*oauth.Manager, error) {
	scopes := []string{oauth.ScopeCardDAV, oauth.ScopeUserinfoEmail}
	dedicated, err := oauth.NewManagerWithScopes(secrets, googleTokensDir(tokensDir, app), logger, scopes)
	if err != nil {
		return nil, err
	}
	if dedicated.HasToken(email) {
		return dedicated, nil
	}
	shared, err := oauth.NewManagerWithScopes(secrets, tokensDir, logger, scopes)
	if err != nil {
		return nil, err
	}
	if shared.TokenMatchesClient(email) {
		return shared, nil
	}
	return dedicated, nil
}
