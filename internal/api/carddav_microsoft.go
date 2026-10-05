package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"golang.org/x/oauth2"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/mscontacts"
)

const cardDAVProviderMicrosoft = "microsoft"

// cardDAVOAuthProvider reports whether provider signs in with OAuth instead
// of a password.
func cardDAVOAuthProvider(provider string) bool {
	return provider == cardDAVProviderGoogle || provider == cardDAVProviderMicrosoft
}

// errMicrosoftContactsAuthorization reports a missing or insufficient
// contacts token. It carries a 401 so sync reports authentication_failed.
var errMicrosoftContactsAuthorization = errors.New("microsoft contacts authorization is required: run msgvault add-carddav --microsoft with your account email")

func (c *CardDAVController) microsoftContactsManager(username string) (*microsoft.GraphManager, error) {
	if c.cfg == nil || c.cfg.Microsoft.ClientID == "" {
		return nil, errors.Join(errMicrosoftContactsAuthorization, errors.New("[microsoft] client_id is not configured"))
	}
	mgr := microsoft.NewGraphContactsManager(c.cfg.Microsoft.ClientID, c.cfg.Microsoft.EffectiveTenantID(),
		c.cfg.Microsoft.EffectiveRedirectURI(), c.cfg.TokensDir(), slog.Default())
	if ok, err := mgr.HasScopes(username); err != nil || !ok {
		return nil, errMicrosoftContactsAuthorization
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
		mgr, err := c.microsoftContactsManager(credential.Username)
		if err != nil {
			return "", errors.Join(err, &carddav.StatusError{StatusCode: http.StatusUnauthorized})
		}
		source, err := mgr.TokenSource(ctx, credential.Username)
		if err != nil {
			return "", errors.Join(err, &carddav.StatusError{StatusCode: http.StatusUnauthorized})
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

// microsoftContactsTokenError marks a revoked or expired refresh token as a
// 401, so sync asks the user to sign in again.
func microsoftContactsTokenError(err error) error {
	err = fmt.Errorf("obtain Microsoft contacts token: %w", err)
	if retrieveErr, ok := errors.AsType[*oauth2.RetrieveError](err); ok && retrieveErr.ErrorCode == "invalid_grant" {
		return errors.Join(err, errMicrosoftContactsAuthorization, &carddav.StatusError{StatusCode: http.StatusUnauthorized})
	}
	return err
}
