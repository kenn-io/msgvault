package carddav

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
)

var (
	// ErrGoogleAuthorizationRequired identifies credentials that need Google sign-in.
	ErrGoogleAuthorizationRequired = errors.New("authorization for Google Contacts is required")
	// ErrGoogleTokenUnavailable identifies an account-wide token acquisition failure.
	ErrGoogleTokenUnavailable = errors.New("token endpoint for Google Contacts is unavailable")
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
	return NewGoogleOAuthManagerWithCredentials(context.Background(), config.OAuthApp{ClientSecrets: secrets}, tokensDir, config.OAuthTokenCommands{}, app, email, logger)
}

func NewGoogleOAuthManagerWithCredentials(ctx context.Context, credentials config.OAuthApp, tokensDir string, commands config.OAuthTokenCommands, app, email string, logger *slog.Logger) (*oauth.Manager, error) {
	scopes := []string{oauth.ScopeCardDAV, oauth.ScopeUserinfoEmail}
	dedicated, err := oauth.NewManagerWithCredentials(ctx, credentials, googleTokensDir(tokensDir, app), commands, logger, scopes)
	if err != nil {
		return nil, err
	}
	info, err := dedicated.InspectToken(ctx, email)
	// A malformed token file counts as absent so a matching shared grant still wins and a new sign-in can repair it.
	if err != nil && !errors.Is(err, oauth.ErrInvalidTokenJSON) {
		return nil, fmt.Errorf("inspect dedicated Google Contacts token: %w", err)
	}
	if info.Exists {
		return dedicated.WithTokenInfo(email, info), nil
	}
	shared, err := oauth.NewManagerWithCredentials(ctx, credentials, tokensDir, commands, logger, scopes)
	if err != nil {
		return nil, err
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
