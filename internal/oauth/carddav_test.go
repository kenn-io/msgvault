package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestCardDAVAuthorizationVerifiesAccountAndPreservesGrants(t *testing.T) {
	for _, tc := range []struct {
		name, actualEmail string
		wantError         bool
	}{
		{"correct account", "person@example.com", false},
		{"wrong account", "other@example.com", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			required := require.New(t)
			const email = "person@example.com"
			mgr := setupTestManager(t, []string{ScopeCardDAV, ScopeUserinfoEmail})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertions.Equal("Bearer new-token", r.Header.Get("Authorization"))
				_, _ = fmt.Fprintf(w, `{"email":%q}`, tc.actualEmail)
			}))
			t.Cleanup(server.Close)
			mgr.profileURL = server.URL
			writeTokenFile(t, mgr, email, oauth2.Token{AccessToken: "old-token", Expiry: time.Now().Add(time.Hour)}, []string{ScopeGmailReadonly, ScopeCalendarReadonly})
			mgr.browserFlowFn = func(context.Context, string, bool) (*oauth2.Token, error) {
				return &oauth2.Token{AccessToken: "new-token", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}, nil
			}
			err := mgr.AuthorizePreservingGrantedScopes(t.Context(), email)
			if tc.wantError {
				required.Error(err)
				assertions.False(mgr.HasScope(email, ScopeCardDAV))
				return
			}
			required.NoError(err)
			assertions.ElementsMatch([]string{ScopeCardDAV, ScopeUserinfoEmail, ScopeGmailReadonly, ScopeCalendarReadonly}, mgr.GrantedScopes(email))
			assertions.Equal("https://www.googleapis.com/oauth2/v2/userinfo", tokenProfileEndpointForScopes(mgr.GrantedScopes(email)).url)
		})
	}
}
