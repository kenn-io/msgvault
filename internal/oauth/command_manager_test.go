package oauth

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"golang.org/x/oauth2"
)

func commandManager(t *testing.T) (*Manager, config.OAuthTokenCommands) {
	t.Helper()
	commands := secretStoreFixture(t)
	mgr, err := NewManagerWithCredentials(t.Context(), config.OAuthApp{ClientSecretsCommand: secretCommand(t, "client")}, t.TempDir(), commands, nil, ScopesGmailReadonly)
	require.NoError(t, err)
	return mgr, commands
}

func TestInspectTokenClassifiesMalformedFileJSON(t *testing.T) {
	required := require.New(t)
	mgr := NewStoredTokenManager(t.TempDir(), config.OAuthTokenCommands{})
	required.NoError(os.WriteFile(mgr.TokenPath("reader@example.com"), []byte("{"), 0600))

	_, err := mgr.InspectToken(t.Context(), "reader@example.com")
	required.ErrorIs(err, ErrInvalidTokenJSON)
}

func TestCommandManagerAuthorizationAndMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr, _ := commandManager(t)
	mgr.browserFlowFn = func(context.Context, string, bool) (*oauth2.Token, error) {
		return &oauth2.Token{AccessToken: "example-access", RefreshToken: "example-refresh", Expiry: time.Now().Add(time.Hour)}, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"emailAddress":"reader@example.com"}`))
	}))
	defer server.Close()
	mgr.profileURL = server.URL
	require.NoError(mgr.Authorize(t.Context(), "reader@example.com"))
	state, err := mgr.InspectToken(t.Context(), "reader@example.com")
	require.NoError(err)
	assert.True(state.Exists)
	assert.Equal("example-client", state.ClientID)
	assert.ElementsMatch(ScopesGmailReadonly, state.Scopes)
	_, err = os.Stat(filepath.Join(mgr.tokensDir, "reader@example.com.json"))
	require.ErrorIs(err, os.ErrNotExist)
	require.NoError(mgr.DeleteToken("reader@example.com"))
	state, err = mgr.InspectToken(t.Context(), "reader@example.com")
	require.NoError(err)
	assert.False(state.Exists)
}
func TestCommandManagerRejectsMalformedAndFailedReads(t *testing.T) {
	mgr, commands := commandManager(t)
	for _, mode := range []string{"empty", "fail", "empty-object"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			commands.ReadCommand = secretCommand(t, mode)
			mgr.tokenStore = NewTokenStore(mgr.tokensDir, commands)
			_, _, err := mgr.prepareAuthorization("reader@example.com", true)
			require.Error(err)
			require.NotContains(err.Error(), "example-private")
			// Only a token file can be malformed in a way a new sign-in repairs.
			require.NotErrorIs(err, ErrInvalidTokenJSON)
			_, err = mgr.TokenSource(t.Context(), "reader@example.com")
			require.Error(err)
			require.NotErrorIs(err, ErrInvalidTokenJSON)
		})
	}
}
func TestCommandManagerSnapshotComparison(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr, _ := commandManager(t)
	require.NoError(mgr.saveToken("reader@example.com", &oauth2.Token{AccessToken: "old"}, Scopes))
	old, err := mgr.loadTokenFile("reader@example.com")
	require.NoError(err)
	require.NoError(mgr.saveToken("reader@example.com", &oauth2.Token{AccessToken: "narrow"}, ScopesGmailReadonly))
	require.ErrorIs(mgr.saveTokenCompared("reader@example.com", &oauth2.Token{AccessToken: "stale"}, Scopes, old), ErrTokenChanged)
	current, err := mgr.loadTokenFile("reader@example.com")
	require.NoError(err)
	assert.Equal("narrow", current.AccessToken)
	assert.ElementsMatch(ScopesGmailReadonly, current.Scopes)
}
func TestCommandManagerListAliasesWithoutSourceRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr, commands := commandManager(t)
	require.NoError(mgr.saveToken("Reader@example.com", &oauth2.Token{AccessToken: "example"}, Scopes))
	emails, err := mgr.EquivalentTokenEmails(t.Context(), "reader@example.com")
	require.NoError(err)
	assert.Equal([]string{"Reader@example.com"}, emails)
	commands.ListCommand = secretCommand(t, "fail")
	mgr.tokenStore = NewTokenStore(mgr.tokensDir, commands)
	_, err = mgr.EquivalentTokenEmails(t.Context(), "reader@example.com")
	assert.Error(err)
}
func TestCommandManagerClientParseErrorsRedacted(t *testing.T) {
	secretStoreFixture(t)
	_, err := NewManagerWithCredentials(t.Context(), config.OAuthApp{ClientSecretsCommand: append(secretCommand(t, "echo"), "example-private-malformed")}, t.TempDir(), config.OAuthTokenCommands{}, nil, Scopes)
	require.ErrorIs(t, err, ErrClientConfig)
	assert.NotContains(t, err.Error(), "example-private")
}

// A later save must not rewrite a token already handed to callers.
func TestTokenSourceReturnedTokenSurvivesLaterSave(t *testing.T) {
	require := require.New(t)
	mgr := NewStoredTokenManager(t.TempDir(), config.OAuthTokenCommands{})
	require.NoError(mgr.saveToken("reader@example.com", &oauth2.Token{AccessToken: "first", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}, Scopes))
	ts, err := mgr.TokenSource(t.Context(), "reader@example.com")
	require.NoError(err)
	first, err := ts.Token()
	require.NoError(err)
	source, ok := ts.(*persistingTokenSource)
	require.True(ok)
	require.NoError(mgr.saveTokenCompared("reader@example.com", &oauth2.Token{AccessToken: "second"}, Scopes, source.expected))
	assert.Equal(t, "first", first.AccessToken)
}

// Expiry-only changes must persist; a failed persistence stays dirty on retry.
func TestCommandManagerRefreshPersistenceRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr, commands := commandManager(t)
	initial := &oauth2.Token{AccessToken: "same", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}
	require.NoError(mgr.saveToken("reader@example.com", initial, ScopesGmailReadonly))
	snapshot, err := mgr.loadTokenFile("reader@example.com")
	require.NoError(err)
	changed := *initial
	changed.Expiry = initial.Expiry.Add(time.Hour)
	commands.WriteCommand = secretCommand(t, "fail")
	mgr.tokenStore = NewTokenStore(mgr.tokensDir, commands)
	source := &persistingTokenSource{manager: mgr, source: oauth2.StaticTokenSource(&changed), email: "reader@example.com", expected: snapshot, ctx: t.Context()}
	_, err = source.Token()
	require.Error(err)
	_, err = source.Token()
	require.Error(err)
	commands.WriteCommand = secretCommand(t, "write")
	mgr.tokenStore = NewTokenStore(mgr.tokensDir, commands)
	_, err = source.Token()
	require.NoError(err)
	saved, err := mgr.loadTokenFile("reader@example.com")
	require.NoError(err)
	assert.True(changed.Expiry.Equal(saved.Expiry))
	assert.Equal(saved.snapshot, source.expected.snapshot)
	var raw map[string]any
	require.NoError(json.Unmarshal(saved.snapshot, &raw))
	assert.Equal("example-client", raw["client_id"])
}

// Two sources sharing one token: when the other saves first, this one continues
// from the stored token instead of failing every later refresh or overwriting it.
func TestCommandManagerRefreshAdoptsNewerStoredToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr, _ := commandManager(t)
	initial := &oauth2.Token{AccessToken: "initial", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Minute)}
	require.NoError(mgr.saveToken("reader@example.com", initial, Scopes))
	snapshot, err := mgr.loadTokenFile("reader@example.com")
	require.NoError(err)
	newer := &oauth2.Token{AccessToken: "newer", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}
	require.NoError(mgr.saveToken("reader@example.com", newer, ScopesGmailReadonly))
	refreshed := &oauth2.Token{AccessToken: "refreshed", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}
	source := &persistingTokenSource{manager: mgr, source: oauth2.StaticTokenSource(refreshed), email: "reader@example.com", expected: snapshot, ctx: t.Context()}
	for range 2 {
		token, err := source.Token()
		require.NoError(err)
		assert.Equal("newer", token.AccessToken)
	}
	stored, err := mgr.loadTokenFile("reader@example.com")
	require.NoError(err)
	assert.Equal("newer", stored.AccessToken)
	assert.ElementsMatch(ScopesGmailReadonly, stored.Scopes)
}

// A newer token from a different OAuth client is left alone rather than adopted.
func TestCommandManagerRefreshKeepsOtherClientToken(t *testing.T) {
	require := require.New(t)
	mgr, _ := commandManager(t)
	require.NoError(mgr.saveToken("reader@example.com", &oauth2.Token{AccessToken: "initial", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Minute)}, Scopes))
	snapshot, err := mgr.loadTokenFile("reader@example.com")
	require.NoError(err)
	other := *mgr
	other.config = &oauth2.Config{ClientID: "other-client"}
	require.NoError(other.saveToken("reader@example.com", &oauth2.Token{AccessToken: "other", RefreshToken: "other-refresh", Expiry: time.Now().Add(time.Hour)}, Scopes))
	refreshed := &oauth2.Token{AccessToken: "refreshed", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}
	source := &persistingTokenSource{manager: mgr, source: oauth2.StaticTokenSource(refreshed), email: "reader@example.com", expected: snapshot, ctx: t.Context()}
	_, err = source.Token()
	require.ErrorIs(err, ErrTokenChanged)
	stored, err := mgr.loadTokenFile("reader@example.com")
	require.NoError(err)
	require.Equal("other", stored.AccessToken)
}

func TestCommandAuthorizationRejectsChangedMetadataSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr, _ := commandManager(t)
	require.NoError(mgr.saveToken("reader@example.com", &oauth2.Token{AccessToken: "before"}, Scopes))
	info, err := mgr.InspectToken(t.Context(), "reader@example.com")
	require.NoError(err)
	scoped := mgr.WithTokenInfo("reader@example.com", info)
	require.NoError(mgr.saveToken("reader@example.com", &oauth2.Token{AccessToken: "narrowed"}, ScopesGmailReadonly))
	_, _, err = scoped.prepareAuthorization("reader@example.com", false)
	assert.ErrorIs(err, ErrTokenChanged)
}

func TestCommandManagerPersistsLaterHTTPRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr, _ := commandManager(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"example-refreshed-%d","refresh_token":"example-refresh","token_type":"Bearer","expires_in":1}`, calls.Add(1))
	}))
	defer server.Close()
	mgr.config.Endpoint = oauth2.Endpoint{TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInParams}
	require.NoError(mgr.saveToken("reader@example.com", &oauth2.Token{AccessToken: "expired", RefreshToken: "example-refresh", Expiry: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}, ScopesGmailReadonly))
	source, err := mgr.TokenSource(t.Context(), "reader@example.com")
	require.NoError(err)
	token, err := source.Token()
	require.NoError(err)
	assert.Equal("example-refreshed-2", token.AccessToken)
	saved, err := mgr.loadTokenFile("reader@example.com")
	require.NoError(err)
	assert.Equal("example-refreshed-2", saved.AccessToken)
	assert.ElementsMatch(ScopesGmailReadonly, saved.Scopes)
}

func TestCommandMetadataKeepsCheckedSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr, commands := commandManager(t)
	require.NoError(mgr.saveToken("reader@example.com", &oauth2.Token{AccessToken: "example"}, ScopesGmailReadonly))
	info, err := mgr.InspectToken(t.Context(), "reader@example.com")
	require.NoError(err)
	selected := mgr.WithTokenInfo("reader@example.com", info)
	commands.ReadCommand = secretCommand(t, "fail")
	selected.tokenStore = NewTokenStore(mgr.tokensDir, commands)
	assert.Equal(info.Scopes, selected.GrantedScopes("reader@example.com"))
	assert.True(selected.HasScopeMetadata("reader@example.com"))
	assert.True(selected.TokenMatchesClient("reader@example.com"))
	_, _, err = selected.prepareAuthorization("reader@example.com", false)
	assert.Error(err)
}

func TestCommandCredentialFreeCleanupPreservesSharedGrant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	commands := secretStoreFixture(t)
	mgr := NewStoredTokenManager(t.TempDir(), commands)
	data := []byte(`{"refresh_token":"example-refresh","client_id":"example-client"}`)
	require.NoError(mgr.store().Write(t.Context(), "Reader@example.com", data))
	require.NoError(mgr.store().Write(t.Context(), "reader@example.com", data))
	shared, err := mgr.EquivalentGrantInUse(t.Context(), "Reader@example.com", []string{"reader@example.com"})
	require.NoError(err)
	assert.True(shared)
	server, revoked := newRevokeServer(t, http.StatusOK, "")
	mgr.revokeURL = server.URL
	require.NoError(mgr.RevokeToken(t.Context(), "Reader@example.com"))
	assert.Equal([]string{"example-refresh"}, *revoked)
	require.NoError(mgr.DeleteToken("Reader@example.com"))
	remaining, err := mgr.InspectToken(t.Context(), "reader@example.com")
	require.NoError(err)
	assert.True(remaining.Exists)
	commands.ReadCommand = secretCommand(t, "fail")
	mgr.tokenStore = NewTokenStore(mgr.tokensDir, commands)
	_, err = mgr.EquivalentGrantInUse(t.Context(), "reader@example.com", []string{"Reader@example.com"})
	assert.Error(err)
}
