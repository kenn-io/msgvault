package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"golang.org/x/oauth2"
)

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
		return fmt.Errorf("exchange Google authorization code: %w", err)
	}
	return f.manager.verifyAndSaveToken(ctx, f.email, token, f.expected)
}
