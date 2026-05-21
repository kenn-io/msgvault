package oauth

import (
	"fmt"
	"log/slog"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// oauthClientID and oauthClientSecret hold the centralized verified
// msgvault OAuth client credentials. They are package vars (not consts)
// so release builds can override them via:
//
//	go build -ldflags "-X github.com/wesm/msgvault/internal/oauth.oauthClientID=..."
//
// Per https://developers.google.com/identity/protocols/oauth2 the desktop
// client secret is "obviously not treated as a secret"; PKCE provides the
// flow security. The values below are the dev project's credentials,
// suitable for contributor builds. Production binaries override both.
var (
	oauthClientID     = "913114107126-tfruv1983bsv811mbjkqjvtd23io5b93.apps.googleusercontent.com"
	oauthClientSecret = "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"
)

// HasEmbeddedCredentials reports whether the package vars are non-empty.
// Forks that strip the values out (or contributors testing the fallback)
// will see this return false, in which case NewEmbeddedManager refuses
// to construct an embedded manager.
func HasEmbeddedCredentials() bool {
	return oauthClientID != "" && oauthClientSecret != ""
}

// EmbeddedConfig returns the oauth2.Config built from the embedded
// credentials. RedirectURL is set later inside Manager.browserFlow when
// the loopback port is known; the rest of the config is fixed here.
func EmbeddedConfig(scopes []string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     oauthClientID,
		ClientSecret: oauthClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       scopes,
	}
}

// NewEmbeddedManager constructs a Manager backed by the centralized
// verified OAuth client. Returns an error when credentials are missing
// (forks, stripped builds, or contributors who blanked the vars
// locally).
func NewEmbeddedManager(tokensDir string, logger *slog.Logger, scopes []string) (*Manager, error) {
	if !HasEmbeddedCredentials() {
		return nil, fmt.Errorf("no embedded OAuth credentials in this build")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		config:     EmbeddedConfig(scopes),
		tokensDir:  tokensDir,
		logger:     logger,
		isEmbedded: true,
	}, nil
}
