package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"golang.org/x/oauth2"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/mscontacts"
)

const cardDAVProviderMicrosoft = "microsoft"

// cardDAVOAuthProvider reports whether provider signs in with OAuth instead
// of a password.
func cardDAVOAuthProvider(provider string) bool {
	return provider == cardDAVProviderGoogle || provider == cardDAVProviderMicrosoft
}

func (c *CardDAVController) microsoftContactsManager(username string) (*microsoft.GraphManager, error) {
	if c.cfg == nil || c.cfg.Microsoft.ClientID == "" {
		return nil, errors.Join(carddav.ErrMicrosoftAuthorizationRequired, errors.New("[microsoft] client_id is not configured"))
	}
	mgr := microsoft.NewGraphContactsManager(c.cfg.Microsoft.ClientID, c.cfg.Microsoft.EffectiveTenantID(),
		c.cfg.Microsoft.EffectiveRedirectURI(), c.cfg.TokensDir(), slog.Default())
	if ok, err := mgr.HasScopes(username); err != nil || !ok {
		return nil, carddav.ErrMicrosoftAuthorizationRequired
	}
	return mgr, nil
}

func (c *CardDAVController) microsoftService(credential carddav.Credential) (cardDAVCandidate, error) {
	if credential.BaseURL != mscontacts.GraphBaseURL {
		return nil, errors.New("use the Microsoft Graph URL for Microsoft contacts")
	}
	token := func(ctx context.Context) (string, error) {
		// Resolve the token on each request, so a new sign-in takes effect
		// without a restart.
		// A missing sign-in carries no HTTP status, so a pending write is kept
		// for after the user signs in again.
		mgr, err := c.microsoftContactsManager(credential.Username)
		if err != nil {
			return "", err
		}
		source, err := mgr.TokenSource(ctx, credential.Username)
		if err != nil {
			return "", errors.Join(err, carddav.ErrMicrosoftAuthorizationRequired)
		}
		accessToken, err := source(ctx)
		if err != nil {
			return "", microsoftContactsTokenError(err)
		}
		return accessToken, nil
	}
	remote := mscontacts.NewRemote(mscontacts.GraphBaseURL, token)
	return carddav.NewRemoteService(c.store, remote).ForConnection(c.connection(), credential.ConnectionGeneration), nil
}

// microsoftContactsTokenError marks a revoked or expired refresh token as
// needing sign-in, and a throttled refresh as a 429, so sync pauses the
// connection.
func microsoftContactsTokenError(err error) error {
	err = fmt.Errorf("obtain Microsoft contacts token: %w", err)
	retrieveErr, ok := errors.AsType[*oauth2.RetrieveError](err)
	switch {
	case !ok:
		// A network failure reaching the token endpoint sends no write.
		return errors.Join(err, carddav.ErrMicrosoftTokenUnavailable, carddav.ErrWriteNotSent)
	case slices.Contains([]string{"invalid_grant", "interaction_required", "consent_required"}, retrieveErr.ErrorCode):
		// Only a new sign-in repairs a revoked token, an MFA prompt or a
		// withdrawn consent.
		return errors.Join(err, carddav.ErrMicrosoftAuthorizationRequired)
	case retrieveErr.Response != nil && retrieveErr.Response.StatusCode == http.StatusTooManyRequests:
		retryAfter := httpretry.RetryAfter(retrieveErr.Response.Header.Get("Retry-After"), 0, time.Hour)
		return errors.Join(err, &carddav.StatusError{StatusCode: http.StatusTooManyRequests, RetryAfter: retryAfter})
	}
	return errors.Join(err, carddav.ErrMicrosoftTokenUnavailable, carddav.ErrWriteNotSent)
}
