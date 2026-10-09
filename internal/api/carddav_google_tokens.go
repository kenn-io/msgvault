package api

import (
	"sync"

	"go.kenn.io/msgvault/internal/carddav"
	"golang.org/x/oauth2"
)

// googleAccessTokens reuses Google Contacts access tokens across CardDAV
// requests. Resolving a token can run credential commands, so the daemon does
// it once per access-token lifetime rather than once per request.
type googleAccessTokens struct {
	mu     sync.Mutex
	tokens map[googleTokenKey]oauth2.Token
}

// googleTokenKey identifies the grant that resolution selects. Connections that
// share an account and OAuth app share one grant.
type googleTokenKey struct {
	username string
	oauthApp string
}

func googleTokenKeyFor(credential carddav.Credential) googleTokenKey {
	return googleTokenKey{username: credential.Username, oauthApp: credential.OAuthApp}
}

// valid returns a cached access token that has not reached its expiry. Tokens
// without an expiry are never cached, so their grant is re-resolved each time.
func (t *googleAccessTokens) valid(key googleTokenKey) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	token, ok := t.tokens[key]
	if !ok || !token.Valid() {
		return "", false
	}
	return token.AccessToken, true
}

func (t *googleAccessTokens) store(key googleTokenKey, token *oauth2.Token) {
	if token.Expiry.IsZero() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tokens == nil {
		t.tokens = make(map[googleTokenKey]oauth2.Token)
	}
	t.tokens[key] = *token
}

// forget drops a cached token so the next request reads the current grant.
func (t *googleAccessTokens) forget(key googleTokenKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.tokens, key)
}
