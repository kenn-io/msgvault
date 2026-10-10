package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/carddav"
	"golang.org/x/oauth2"
)

func TestGoogleAccessTokensRejectStoreAfterForget(t *testing.T) {
	assert := assert.New(t)
	var tokens googleAccessTokens
	key := googleTokenKeyFor(carddav.Credential{Username: "person@example.com", OAuthApp: "work"})
	old := &oauth2.Token{AccessToken: "old-access", Expiry: time.Now().Add(time.Hour)}

	// A lookup misses the cache, then a sign-in clears it before the lookup stores.
	_, generation, ok := tokens.valid(key)
	assert.False(ok)
	tokens.forget(key)
	tokens.store(key, generation, old)
	_, _, ok = tokens.valid(key)
	assert.False(ok, "a lookup that began before the sign-in must not restore the old token")

	_, generation, _ = tokens.valid(key)
	current := &oauth2.Token{AccessToken: "current-access", Expiry: time.Now().Add(time.Hour)}
	tokens.store(key, generation, current)
	access, _, ok := tokens.valid(key)
	assert.True(ok)
	assert.Equal("current-access", access)

	other := googleTokenKeyFor(carddav.Credential{Username: "person@example.com", OAuthApp: "personal"})
	tokens.forget(other)
	access, _, ok = tokens.valid(key)
	assert.True(ok, "forgetting another grant keeps this one")
	assert.Equal("current-access", access)
}

func TestGoogleAccessTokensSkipTokensWithoutExpiry(t *testing.T) {
	var tokens googleAccessTokens
	key := googleTokenKeyFor(carddav.Credential{Username: "person@example.com"})
	_, generation, _ := tokens.valid(key)
	tokens.store(key, generation, &oauth2.Token{AccessToken: "no-expiry"})
	_, _, ok := tokens.valid(key)
	assert.False(t, ok)
}
