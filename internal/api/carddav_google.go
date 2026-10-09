package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/mscontacts"
	"go.kenn.io/msgvault/internal/oauth"
	"golang.org/x/oauth2"
)

func normalizeCardDAVAccountRequest(req CardDAVAccountRequest) CardDAVAccountRequest {
	switch req.Provider {
	case cardDAVProviderGoogle:
		req.BaseURL = carddav.GoogleDiscoveryURL
		req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	case cardDAVProviderMicrosoft:
		req.BaseURL = mscontacts.GraphBaseURL
		req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	}
	return req
}

func cardDAVCredentialMatchesConfig(credential carddav.Credential, cfg config.CardDAVConfig) bool {
	return credential.OAuthApp == cfg.OAuthApp &&
		((cfg.Provider == "" && !credential.OAuth()) || (cfg.Provider == cardDAVProviderGoogle && credential.Google) ||
			(cfg.Provider == cardDAVProviderMicrosoft && credential.Microsoft))
}

func (c *CardDAVController) credentialForRequest(ctx context.Context, req CardDAVAccountRequest) (carddav.Credential, error) {
	credential := carddav.Credential{
		BaseURL: req.BaseURL, Username: req.Username, OAuthApp: req.OAuthApp,
		Google: req.Provider == cardDAVProviderGoogle, Microsoft: req.Provider == cardDAVProviderMicrosoft,
	}
	if credential.OAuth() {
		return credential, nil
	}
	password, err := c.passwordForRequest(ctx, req)
	credential.Password = password
	return credential, err
}

func (c *CardDAVController) serviceForCredential(credential carddav.Credential, configured config.CardDAVConfig) (cardDAVCandidate, error) {
	if credential.Microsoft {
		return c.microsoftService(credential)
	}
	if !credential.Google {
		candidate, err := c.factory(c.store, configured, credential.Password)
		if err != nil {
			return nil, err
		}
		return c.scopedCandidate(candidate, credential.ConnectionGeneration), nil
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
	return carddav.NewGoogleService(c.store, client).ForConnection(c.connection(), credential.ConnectionGeneration), nil
}

func (c *CardDAVController) googleBearerToken(ctx context.Context, credential carddav.Credential) (string, error) {
	// Resolve the token directory on each request: CLI authorization can
	// switch between a shared mail token and a dedicated Contacts token.
	mgr, err := c.googleOAuthManager(ctx, credential)
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

// googleCardDAVTokenError asks for sign-in only when a new grant fixes the
// failure. Every other failure is temporary, so schedules keep retrying.
// No contacts write was sent, so a pending write can be retried.
func googleCardDAVTokenError(err error) error {
	if errors.Is(err, carddav.ErrGoogleAuthorizationRequired) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, oauth.ErrInvalidTokenJSON) || errors.Is(err, oauth.ErrTokenUnrefreshable) {
		return fmt.Errorf("%w: %w", carddav.ErrGoogleAuthorizationRequired, err)
	}
	status := &carddav.StatusError{StatusCode: http.StatusBadGateway}
	if retrieveErr, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		if retrieveErr.ErrorCode == "invalid_grant" || retrieveErr.ErrorCode == "invalid_client" {
			return fmt.Errorf("%w: %w", carddav.ErrGoogleAuthorizationRequired, err)
		}
		if retrieveErr.Response != nil {
			status.StatusCode = retrieveErr.Response.StatusCode
			if value := strings.TrimSpace(retrieveErr.Response.Header.Get("Retry-After")); value != "" {
				status.RetryAfter = httpretry.RetryAfter(value, 0, time.Hour)
			}
		}
	}
	return fmt.Errorf("obtain Google access token: %w", errors.Join(err, carddav.ErrGoogleTokenUnavailable, carddav.ErrWriteNotSent, status))
}

func (c *CardDAVController) googleOAuthManager(ctx context.Context, credential carddav.Credential) (*oauth.Manager, error) {
	secrets, err := c.cfg.OAuth.CredentialsFor(credential.OAuthApp)
	if err != nil {
		return nil, errors.Join(carddav.ErrGoogleAuthorizationRequired, err)
	}
	mgr, err := carddav.NewGoogleOAuthManagerWithCredentials(ctx, secrets, c.cfg.TokensDir(), c.cfg.OAuth.Tokens, credential.OAuthApp, credential.Username, slog.Default())
	if errors.Is(err, oauth.ErrClientConfig) {
		return nil, errors.Join(carddav.ErrGoogleAuthorizationRequired, err)
	}
	if err != nil {
		return nil, googleCardDAVTokenError(err)
	}
	info, err := mgr.SelectedTokenInfo(ctx, credential.Username)
	if err != nil {
		return nil, googleCardDAVTokenError(err)
	}
	if !info.ClientMatches || !info.HasScope(oauth.ScopeCardDAV) {
		return nil, carddav.ErrGoogleAuthorizationRequired
	}
	return mgr, nil
}
