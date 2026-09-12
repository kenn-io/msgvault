package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestWebAuthorizationExchangesPKCEAndPreservesExistingAccess(t *testing.T) {
	assertions := assert.New(t)
	required := require.New(t)
	const email = "person@example.com"
	mgr := setupTestManager(t, []string{ScopeCardDAV, ScopeUserinfoEmail})
	mgr.config.ClientID = "previous-client"
	required.NoError(mgr.saveToken(email, &oauth2.Token{AccessToken: "old", Expiry: time.Now().Add(time.Hour)}, []string{ScopeGmailReadonly}))
	mgr.config.ClientID = "synthetic-client"
	var challenge string
	exchanges := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/profile" {
			assertions.Equal("Bearer synthetic-access", r.Header.Get("Authorization"))
			_, _ = fmt.Fprintf(w, `{"email":%q}`, email)
			return
		}
		exchanges++
		if !assertions.NoError(r.ParseForm()) {
			return
		}
		assertions.Equal("one-time-code", r.Form.Get("code"))
		assertions.Equal("https://archive.example/", r.Form.Get("redirect_uri"))
		digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		assertions.Equal(challenge, base64.RawURLEncoding.EncodeToString(digest[:]))
		_, _ = fmt.Fprintf(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","expires_in":3600,"scope":%q}`, ScopeCardDAV+" "+ScopeUserinfoEmail+" "+ScopeGmailReadonly)
	}))
	t.Cleanup(server.Close)
	mgr.config.Endpoint = oauth2.Endpoint{AuthURL: "https://accounts.example/authorize", TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}
	mgr.profileURL = server.URL + "/profile"
	flow, err := mgr.BeginWebAuthorization(email, "https://archive.example/")
	required.NoError(err)
	u, err := url.Parse(flow.URL)
	required.NoError(err)
	challenge = u.Query().Get("code_challenge")
	assertions.NotEmpty(challenge)
	assertions.Equal("S256", u.Query().Get("code_challenge_method"))
	assertions.Equal(email, u.Query().Get("login_hint"))
	required.Error(flow.Complete(t.Context(), "wrong-state", "one-time-code"))
	assertions.Zero(exchanges)
	required.NoError(flow.Complete(t.Context(), flow.State, "one-time-code"))
	assertions.Equal(1, exchanges)
	token, err := mgr.loadTokenFile(email)
	required.NoError(err)
	assertions.Equal("synthetic-client", token.ClientID)
	assertions.Equal("synthetic-refresh", token.RefreshToken)
	assertions.ElementsMatch([]string{ScopeCardDAV, ScopeUserinfoEmail, ScopeGmailReadonly}, mgr.GrantedScopes(email))
}

func TestWebAuthorizationTokenProvenance(t *testing.T) {
	const email = "person@example.com"
	for _, tc := range []struct {
		name     string
		clientID string
		scopes   []string
	}{
		{name: "legacy token"},
		{name: "unknown client with recorded scopes", scopes: []string{ScopeGmailReadonly}},
		{name: "same client", clientID: "selected-client", scopes: []string{ScopeGmailReadonly}},
		{name: "different client", clientID: "other-client", scopes: []string{ScopeGmailReadonly}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			required := require.New(t)
			mgr := setupTestManager(t, []string{ScopeCardDAV, ScopeUserinfoEmail})
			mgr.config.ClientID = tc.clientID
			required.NoError(mgr.saveToken(email, &oauth2.Token{AccessToken: "synthetic-token"}, tc.scopes))
			mgr.config.ClientID = "selected-client"
			mgr.config.Endpoint.AuthURL = "https://accounts.example/authorize"
			flow, err := mgr.BeginWebAuthorization(email, "https://archive.example/")
			required.NoError(err)
			u, err := url.Parse(flow.URL)
			required.NoError(err)
			want := append([]string{ScopeCardDAV, ScopeUserinfoEmail}, tc.scopes...)
			assertions.ElementsMatch(want, strings.Fields(u.Query().Get("scope")))
			token, err := mgr.loadTokenFile(email)
			required.NoError(err)
			assertions.Equal(tc.clientID, token.ClientID)
			assertions.Equal("synthetic-token", token.AccessToken)
		})
	}
}

func TestWebAuthorizationCannotOverwriteNewerAuthorization(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			assertions := assert.New(t)
			required := require.New(t)
			const email = "person@example.com"
			oldScopes := []string{ScopeCardDAV, ScopeUserinfoEmail, ScopeGmailReadonly}
			newScopes := append(append([]string(nil), oldScopes...), ScopeCalendarReadonly)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/profile" {
					_, _ = fmt.Fprintf(w, `{"email":%q}`, email)
					return
				}
				if !assertions.NoError(r.ParseForm()) {
					return
				}
				scopes, prefix := oldScopes, "old"
				if r.Form.Get("code") == "new" {
					scopes, prefix = newScopes, "new"
				}
				_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"token_type":"Bearer","expires_in":3600,"scope":%q}`,
					prefix+"-access", prefix+"-refresh", strings.Join(scopes, " "))
			}))
			t.Cleanup(server.Close)
			mgr := setupTestManager(t, oldScopes)
			mgr.config.Endpoint = oauth2.Endpoint{AuthURL: "https://accounts.example/authorize", TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInParams}
			mgr.profileURL = server.URL + "/profile"
			if existing {
				required.NoError(mgr.saveToken(email, &oauth2.Token{AccessToken: "initial-access"}, []string{ScopeGmailReadonly}))
			}
			older, err := mgr.BeginWebAuthorization(email, "https://archive.example/")
			required.NoError(err)
			newer, err := mgr.withScopes(newScopes).BeginWebAuthorization(email, "https://archive.example/")
			required.NoError(err)
			required.NoError(newer.Complete(t.Context(), newer.State, "new"))
			before, err := mgr.loadTokenFile(email)
			required.NoError(err)

			required.ErrorIs(older.Complete(t.Context(), older.State, "old"), ErrTokenChanged)
			after, err := mgr.loadTokenFile(email)
			required.NoError(err)
			assertions.Equal(before.snapshot, after.snapshot, "keep the newer token and its Calendar permission")
		})
	}
}
