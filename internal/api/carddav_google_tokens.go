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
	mu      sync.Mutex
	entries map[googleTokenKey]googleTokenEntry
}

// googleTokenEntry counts invalidations so a lookup that started before one
// cannot store the token it read from the older grant.
type googleTokenEntry struct {
	token      oauth2.Token
	cached     bool
	generation uint64
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

// valid returns a cached access token that has not reached its expiry. When
// none exists, it returns the generation that a later store must still match.
func (t *googleAccessTokens) valid(key googleTokenKey) (string, uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[key]
	if !entry.cached || !entry.token.Valid() {
		return "", entry.generation, false
	}
	return entry.token.AccessToken, entry.generation, true
}

// store caches token unless the key was forgotten after the lookup began.
// Tokens without an expiry are never cached, so their grant is re-resolved.
func (t *googleAccessTokens) store(key googleTokenKey, generation uint64, token *oauth2.Token) {
	if token.Expiry.IsZero() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[key]
	if entry.generation != generation {
		return
	}
	if t.entries == nil {
		t.entries = make(map[googleTokenKey]googleTokenEntry)
	}
	t.entries[key] = googleTokenEntry{token: *token, cached: true, generation: generation}
}

// forget drops a cached token and rejects stores from lookups already running,
// so the next request reads the current grant.
func (t *googleAccessTokens) forget(key googleTokenKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[googleTokenKey]googleTokenEntry)
	}
	t.entries[key] = googleTokenEntry{generation: t.entries[key].generation + 1}
}
