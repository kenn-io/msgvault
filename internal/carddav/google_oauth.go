package carddav

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
)

var (
	// ErrGoogleAuthorizationRequired identifies credentials that need Google sign-in.
	ErrGoogleAuthorizationRequired = errors.New("authorization for Google Contacts is required")
	// ErrMicrosoftAuthorizationRequired identifies credentials that need
	// Microsoft sign-in.
	ErrMicrosoftAuthorizationRequired = errors.New("authorization for Microsoft contacts is required")
	// ErrGoogleTokenUnavailable identifies an account-wide token acquisition failure.
	ErrGoogleTokenUnavailable = errors.New("token endpoint for Google Contacts is unavailable")
	// ErrMicrosoftTokenUnavailable identifies an account-wide failure to get a
	// Microsoft contacts token.
	ErrMicrosoftTokenUnavailable = errors.New("token endpoint for Microsoft contacts is unavailable")
)

// googleTokenNamespace separates CardDAV authorizations by configured OAuth app.
// Hashing the app name keeps arbitrary configuration keys out of path segments.
func googleTokenNamespace(app string) string {
	return fmt.Sprintf("carddav-google/%x", sha256.Sum256([]byte(app)))
}

// NewGoogleOAuthManagerWithCredentials reuses a mail/calendar authorization only when its
// recorded client matches the selected app. An existing CardDAV authorization
// takes precedence so later mail setup cannot switch the connection's token.
func NewGoogleOAuthManagerWithCredentials(
	ctx context.Context, credentials config.OAuthApp, tokensDir string,
	commands config.OAuthTokenCommands, app, email string, logger *slog.Logger,
) (*oauth.Manager, error) {
	scopes := []string{oauth.ScopeCardDAV, oauth.ScopeUserinfoEmail}
	shared, err := oauth.NewManagerWithCredentials(ctx, credentials, tokensDir, commands, logger, scopes)
	if err != nil {
		return nil, err
	}
	dedicated := shared.WithTokenNamespace(googleTokenNamespace(app))
	info, err := dedicated.InspectToken(ctx, email)
	// A malformed token file counts as absent so a matching shared grant still
	// wins and a new sign-in can repair it.
	if err != nil && !errors.Is(err, oauth.ErrInvalidTokenJSON) {
		return nil, fmt.Errorf("inspect dedicated Google Contacts token: %w", err)
	}
	if info.Exists {
		return dedicated.WithTokenInfo(email, info), nil
	}
	sharedInfo, err := shared.InspectToken(ctx, email)
	if err != nil && !errors.Is(err, oauth.ErrInvalidTokenJSON) {
		return nil, fmt.Errorf("inspect shared Google Contacts token: %w", err)
	}
	if sharedInfo.ClientMatches {
		return shared.WithTokenInfo(email, sharedInfo), nil
	}
	return dedicated.WithTokenInfo(email, info), nil
}
