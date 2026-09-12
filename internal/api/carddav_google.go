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
	"time"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/httpretry"
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
	return credential.OAuthApp == cfg.OAuthApp &&
		((cfg.Provider == "" && !credential.Google) || (cfg.Provider == "google" && credential.Google))
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
		if retrieveErr.ErrorCode == "invalid_grant" {
			return fmt.Errorf("%w: %w", carddav.ErrGoogleAuthorizationRequired, err)
		}
		code := retrieveErr.Response.StatusCode
		var retryAfter time.Duration
		if value := strings.TrimSpace(retrieveErr.Response.Header.Get("Retry-After")); value != "" {
			retryAfter = httpretry.RetryAfter(value, 0, time.Hour)
		}
		return fmt.Errorf("obtain Google access token: %w", errors.Join(
			err,
			carddav.ErrGoogleTokenUnavailable,
			&carddav.StatusError{StatusCode: code, RetryAfter: retryAfter},
		))
	}
	// oauth2 formats response-body read failures with %v, losing the cause.
	bodyReadFailure := strings.Contains(err.Error(), "oauth2: cannot fetch token: ")
	if _, ok := errors.AsType[net.Error](err); ok || syncerr.IsTransientNetwork(err) || errors.Is(err, context.Canceled) || bodyReadFailure {
		return fmt.Errorf("obtain Google access token: %w", errors.Join(
			err, carddav.ErrGoogleTokenUnavailable, &carddav.StatusError{StatusCode: http.StatusBadGateway},
		))
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
