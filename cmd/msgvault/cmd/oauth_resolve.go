package cmd

import (
	"fmt"
	"log/slog"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
)

// resolveOAuthManager builds the *oauth.Manager appropriate for the
// account+config+scopes triple. Resolution order:
//
//  1. Named BYO: appName is non-empty and cfg.OAuth.Apps[appName] has
//     client_secrets set — use that BYO OAuth client.
//  2. (If appName is non-empty but no client_secrets is registered for
//     it) — return an error rather than falling through to embedded,
//     because the user explicitly named a binding that doesn't exist.
//  3. Global BYO: appName is empty and cfg.OAuth.ClientSecrets is set —
//     use the global BYO client.
//  4. Embedded: otherwise, use the centralized verified client. On the
//     embedded path the manager is always built with oauth.ScopesEmbedded,
//     ignoring the caller's per-call scope choice, because the embedded
//     grant is broader than any per-call need.
//
// Callers handle service-account resolution themselves (via
// cfg.OAuth.ServiceAccountKeyFor(appName)) before calling this helper,
// because *oauth.Manager and the service-account manager have
// different interfaces.
func resolveOAuthManager(
	cfg *config.Config,
	appName string,
	scopes []string,
	logger *slog.Logger,
) (*oauth.Manager, error) {
	if appName != "" {
		app, ok := cfg.OAuth.Apps[appName]
		if !ok || app.ClientSecrets == "" {
			return nil, fmt.Errorf("OAuth app %q not configured: add [oauth.apps.%s] client_secrets to config.toml, or rebind the account with 'msgvault add-account <email>' (without --oauth-app) to use the embedded client", appName, appName)
		}
		return oauth.NewManagerWithScopes(app.ClientSecrets, cfg.TokensDir(), logger, scopes)
	}

	if cfg.OAuth.ClientSecrets != "" {
		return oauth.NewManagerWithScopes(cfg.OAuth.ClientSecrets, cfg.TokensDir(), logger, scopes)
	}

	return oauth.NewEmbeddedManager(cfg.TokensDir(), logger, oauth.ScopesEmbedded)
}
