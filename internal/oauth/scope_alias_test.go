package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestGoogleScopeAliases(t *testing.T) {
	assertions := assert.New(t)
	expandedProfile := "https://www.googleapis.com/auth/userinfo.profile"
	canonical := []string{ScopeUserinfoEmail, expandedProfile, "openid"}
	assertions.Equal(canonical, normalizedScopeList([]string{" email ", "profile", "openid", ScopeUserinfoEmail, expandedProfile}))
	for _, pair := range [][2]string{{"email", ScopeUserinfoEmail}, {"profile", expandedProfile}} {
		for _, direction := range [][2]string{pair, {pair[1], pair[0]}} {
			assertions.Empty(missingScopes([]string{direction[0]}, []string{direction[1]}))
			assertions.True(GrantCoversAnyScope([]string{direction[0]}, []string{direction[1]}))
		}
	}
	assertions.Equal([]string{"openid"}, missingScopes([]string{"openid"}, []string{"email", "profile"}))
	assertions.Equal([]string{ScopeGmailModify}, missingScopes([]string{ScopeGmailModify}, []string{ScopeGmailReadonly}))
}

func TestAuthorizeGoogleScopeAliases(t *testing.T) {
	for _, expandedResponse := range []bool{true, false} {
		t.Run(strconv.FormatBool(expandedResponse), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			short := []string{ScopeCalendarReadonly, "email", "profile", "openid"}
			expanded := []string{ScopeCalendarReadonly, ScopeUserinfoEmail, "https://www.googleapis.com/auth/userinfo.profile", "openid"}
			required, granted := short, expanded
			if !expandedResponse {
				required, granted = expanded, short
			}
			mgr := setupTestManager(t, required)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer alias-token", r.Header.Get("Authorization"))
				_, _ = fmt.Fprint(w, `{"email":"person@example.com"}`)
			}))
			defer srv.Close()
			mgr.profileURL = srv.URL
			mgr.browserFlowFn = func(context.Context, string, bool) (*oauth2.Token, error) {
				return (&oauth2.Token{AccessToken: "alias-token", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}).WithExtra(map[string]any{"scope": strings.Join(granted, " ")}), nil
			}
			requirements.NoError(mgr.Authorize(context.Background(), "person@example.com"))
			tf, err := mgr.loadTokenFile("person@example.com")
			requirements.NoError(err)
			assertions.Equal(expanded, tf.Scopes)
			assertions.True(mgr.HasScope("person@example.com", "email"))
			assertions.True(mgr.HasScope("person@example.com", ScopeUserinfoEmail))
		})
	}
}

func TestLegacyGoogleScopeAliases(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	mgr := setupTestManager(t, ScopesCalendar)
	// Seed a historical token directly so saveToken cannot normalize the fixture.
	requirements.NoError(os.WriteFile(mgr.tokenPath("person@example.com"), []byte(`{"access_token":"legacy","scopes":["email","profile","openid"]}`), 0600))
	assertions.True(mgr.HasScope("person@example.com", ScopeUserinfoEmail))
	assertions.Equal([]string{ScopeUserinfoEmail, "https://www.googleapis.com/auth/userinfo.profile", "openid"}, mgr.GrantedScopes("person@example.com"))
	assertions.Contains(tokenProfileEndpointForScopes([]string{"email"}).url, "userinfo")
}
