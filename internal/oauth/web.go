package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
	"golang.org/x/oauth2"
)

// AuthorizationUnavailableError identifies an OAuth provider failure that a
// fresh authorization attempt may retry without changing the submitted data.
type AuthorizationUnavailableError struct {
	Cause      error
	RetryAfter time.Duration
}

func (e *AuthorizationUnavailableError) Error() string { return e.Cause.Error() }
func (e *AuthorizationUnavailableError) Unwrap() error { return e.Cause }

func authorizationProviderError(err error, response *http.Response, errorCode string) error {
	if err == nil {
		return nil
	}
	var retryAfter time.Duration
	if response != nil {
		if value := strings.TrimSpace(response.Header.Get("Retry-After")); value != "" {
			retryAfter = httpretry.RetryAfter(value, 0, time.Hour)
		}
		status := response.StatusCode
		if retryAfter > 0 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
			return &AuthorizationUnavailableError{Cause: err, RetryAfter: retryAfter}
		}
	}
	if errorCode == "temporarily_unavailable" || errorCode == "server_error" {
		return &AuthorizationUnavailableError{Cause: err, RetryAfter: retryAfter}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && !errors.Is(err, context.Canceled) {
		return &AuthorizationUnavailableError{Cause: err}
	}
	return err
}

// WebAuthorization retains the verifier and expected account on the daemon
// while the user's browser visits the authorization server.
type WebAuthorization struct {
	URL      string
	State    string
	manager  *Manager
	email    string
	verifier string
	expected *tokenFile
}

// BeginWebAuthorization builds an authorization-code request with PKCE. The
// caller owns expiration and one-time consumption of the returned flow.
func (m *Manager) BeginWebAuthorization(email, redirectURI string) (*WebAuthorization, error) {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" ||
		(u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1"))) {
		return nil, errors.New("OAuth callback requires HTTPS or a loopback HTTP address")
	}
	scoped, expected, err := m.prepareAuthorization(email, true)
	if err != nil {
		return nil, err
	}
	scoped.config.RedirectURL = redirectURI
	flow := &WebAuthorization{State: "msgvault-carddav-" + oauth2.GenerateVerifier(), manager: scoped, email: email, verifier: oauth2.GenerateVerifier(), expected: expected}
	flow.URL = scoped.config.AuthCodeURL(flow.State, oauth2.AccessTypeOffline, oauth2.ApprovalForce,
		oauth2.SetAuthURLParam("login_hint", email), oauth2.S256ChallengeOption(flow.verifier))
	return flow, nil
}

// Complete exchanges a one-time code and applies the same account and scope
// verification as terminal authorization before publishing the token.
func (f *WebAuthorization) Complete(ctx context.Context, state, code string) error {
	if state != f.State || code == "" {
		return errors.New("invalid OAuth callback")
	}
	token, err := f.manager.config.Exchange(withRefreshHTTPClient(ctx), code, oauth2.VerifierOption(f.verifier))
	if err != nil {
		var response *http.Response
		var errorCode string
		if retrieveErr, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
			response = retrieveErr.Response
			errorCode = retrieveErr.ErrorCode
		}
		return fmt.Errorf("exchange Google authorization code: %w", authorizationProviderError(err, response, errorCode))
	}
	return f.manager.verifyAndSaveToken(ctx, f.email, token, f.expected)
}
