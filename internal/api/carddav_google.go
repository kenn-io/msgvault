package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/syncerr"
	"golang.org/x/oauth2"
)

func normalizeCardDAVAccountRequest(req CardDAVAccountRequest) CardDAVAccountRequest {
	if req.Provider == "google" {
		req.BaseURL = carddav.GoogleDiscoveryURL
		req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	}
	return req
}

func cardDAVCredentialMatchesConfig(credential carddav.Credential, cfg config.CardDAVConfig) bool {
	return credential.Google == (cfg.Provider == "google") && credential.OAuthApp == cfg.OAuthApp
}

func (c *CardDAVController) credentialForRequest(ctx context.Context, req CardDAVAccountRequest) (carddav.Credential, error) {
	credential := carddav.Credential{BaseURL: req.BaseURL, Username: req.Username, Google: req.Provider == "google", OAuthApp: req.OAuthApp}
	if credential.Google {
		return credential, nil
	}
	password, err := c.passwordForRequest(ctx, req)
	credential.Password = password
	return credential, err
}

func (c *CardDAVController) serviceForCredential(credential carddav.Credential) (cardDAVCandidate, error) {
	if !credential.Google {
		return c.factory(c.store, credential.BaseURL, credential.Username, credential.Password)
	}
	if credential.BaseURL != carddav.GoogleDiscoveryURL {
		return nil, errors.New("use Google's discovery URL for Google Contacts")
	}
	origin, err := url.Parse(carddav.GoogleDiscoveryURL)
	if err != nil {
		return nil, fmt.Errorf("parse Google discovery URL: %w", err)
	}
	client, err := carddav.NewClient(carddav.ClientOptions{
		CredentialOrigin: origin,
		BearerToken: func(ctx context.Context) (string, error) {
			return c.googleBearerToken(ctx, credential)
		},
	})
	if err != nil {
		return nil, err
	}
	return carddav.NewGoogleService(c.store, client), nil
}

func (c *CardDAVController) googleBearerToken(ctx context.Context, credential carddav.Credential) (string, error) {
	// Resolve the token directory on each request: CLI authorization can
	// switch between a shared mail token and a dedicated Contacts token.
	mgr, err := c.googleOAuthManager(credential)
	if err != nil {
		return "", err
	}
	ts, err := mgr.TokenSource(ctx, credential.Username)
	if err != nil {
		return "", googleCardDAVTokenError(err)
	}
	token, err := ts.Token()
	if err != nil {
		return "", googleCardDAVTokenError(err)
	}
	return token.AccessToken, nil
}

func googleCardDAVTokenError(err error) error {
	if retrieveErr, ok := errors.AsType[*oauth2.RetrieveError](err); ok && retrieveErr.Response != nil {
		code := retrieveErr.Response.StatusCode
		if code == http.StatusTooManyRequests || code >= http.StatusInternalServerError {
			return fmt.Errorf("obtain Google access token: %w", errors.Join(err, &carddav.StatusError{StatusCode: code}))
		}
	} else {
		// oauth2 formats response-body read failures with %v, losing the cause.
		bodyReadFailure := strings.Contains(err.Error(), "oauth2: cannot fetch token: ")
		if _, ok := errors.AsType[net.Error](err); ok || syncerr.IsTransientNetwork(err) || errors.Is(err, context.Canceled) || bodyReadFailure {
			return fmt.Errorf("obtain Google access token: %w", errors.Join(err, &carddav.StatusError{StatusCode: http.StatusBadGateway}))
		}
	}
	return fmt.Errorf("%w: %w", carddav.ErrGoogleAuthorizationRequired, err)
}

func (c *CardDAVController) googleOAuthManager(credential carddav.Credential) (*oauth.Manager, error) {
	secrets, err := c.cfg.OAuth.ClientSecretsFor(credential.OAuthApp)
	if err != nil {
		return nil, errors.Join(carddav.ErrGoogleAuthorizationRequired, err)
	}
	mgr, err := carddav.NewGoogleOAuthManager(secrets, c.cfg.TokensDir(), credential.OAuthApp, credential.Username, slog.Default())
	if err != nil {
		return nil, errors.Join(carddav.ErrGoogleAuthorizationRequired, err)
	}
	if !mgr.TokenMatchesClient(credential.Username) || !mgr.HasScope(credential.Username, oauth.ScopeCardDAV) {
		return nil, carddav.ErrGoogleAuthorizationRequired
	}
	return mgr, nil
}
