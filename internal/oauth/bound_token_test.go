package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"golang.org/x/oauth2"
)

func TestCheckedTokenSourceUsesExactAccountSnapshot(t *testing.T) {
	for _, command := range []bool{false, true} {
		t.Run(strconv.FormatBool(command), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			mgr := NewStoredTokenManager(t.TempDir(), config.OAuthTokenCommands{})
			if command {
				mgr, _ = commandManager(t)
			}
			mgr.config.Scopes = []string{ScopeUserinfoEmail, "https://www.googleapis.com/auth/business.manage"}
			mgr.config.ClientID = "example-client"
			require.NoError(mgr.saveToken("owner@example.com", &oauth2.Token{AccessToken: "checked", RefreshToken: "grant", Expiry: time.Now().Add(time.Hour)}, mgr.config.Scopes))
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal("Bearer checked", r.Header.Get("Authorization"))
				_, err := w.Write([]byte(`{"email":"owner@example.com"}`))
				if !assert.NoError(err) {
					return
				}
			}))
			defer server.Close()
			mgr.profileURL = server.URL
			source, binding, err := mgr.CheckedTokenSource(t.Context(), "owner@example.com", mgr.config.Scopes)
			require.NoError(err)
			require.NotEmpty(binding)
			assert.Equal(1, calls)
			require.NoError(mgr.saveToken("owner@example.com", &oauth2.Token{AccessToken: "replacement", RefreshToken: "other", Expiry: time.Now().Add(time.Hour)}, mgr.config.Scopes))
			token, err := source.Token()
			require.NoError(err)
			assert.Equal("checked", token.AccessToken)
			_, _, err = mgr.CheckedTokenSource(t.Context(), "owner@example.com", []string{"missing"})
			require.Error(err)
			assert.Equal(1, calls)
		})
	}
}
func TestCheckedTokenSourceRejectsActualAccountAndClient(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr := NewStoredTokenManager(t.TempDir(), config.OAuthTokenCommands{})
	mgr.config.ClientID = "example-client"
	mgr.config.Scopes = []string{ScopeUserinfoEmail}
	require.NoError(mgr.saveToken("owner@example.com", &oauth2.Token{AccessToken: "checked", RefreshToken: "grant", Expiry: time.Now().Add(time.Hour)}, mgr.config.Scopes))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		email := "someone@example.com"
		if calls == 2 {
			email = "owner@example.com"
		}
		_, err := w.Write([]byte(`{"email":"` + email + `","private":"CANARY"}`))
		if !assert.NoError(err) {
			return
		}
	}))
	defer server.Close()
	mgr.profileURL = server.URL
	_, _, err := mgr.CheckedTokenSource(t.Context(), "owner@example.com", mgr.config.Scopes)
	require.Error(err)
	assert.NotContains(err.Error(), "CANARY")
	assert.NotContains(err.Error(), "someone")
	mgr.config.ClientID = "different"
	_, _, err = mgr.CheckedTokenSource(t.Context(), "owner@example.com", mgr.config.Scopes)
	require.Error(err)
}
func TestCheckedTokenSourceScopeUsesUserinfo(t *testing.T) {
	endpoint := tokenProfileEndpointForScopes([]string{ScopeUserinfoEmail, "https://www.googleapis.com/auth/business.manage"})
	assert.Equal(t, "https://www.googleapis.com/oauth2/v2/userinfo", endpoint.url)
	_ = context.Canceled
}
