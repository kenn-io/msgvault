package oauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestRefreshCannotOverwriteNewAuthorization(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			assertions := assert.New(t)
			required := require.New(t)
			const email = "person@example.com"
			refreshStarted := make(chan struct{})
			resumeRefresh := make(chan struct{})
			release := sync.OnceFunc(func() { close(resumeRefresh) })
			defer release()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/profile" {
					_, _ = fmt.Fprint(w, `{"email":"person@example.com"}`)
					return
				}
				if err := r.ParseForm(); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if r.Form.Get("grant_type") == "refresh_token" {
					close(refreshStarted)
					select {
					case <-resumeRefresh:
					case <-r.Context().Done():
						return
					}
					_, _ = fmt.Fprint(w, `{"access_token":"stale-refreshed-access","refresh_token":"stale-refresh","token_type":"Bearer","expires_in":3600}`)
					return
				}
				_, _ = fmt.Fprintf(w, `{"access_token":"new-authorization","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600,"scope":%q}`, ScopeCardDAV+" "+ScopeUserinfoEmail+" "+ScopeGmailReadonly)
			}))
			t.Cleanup(server.Close)
			mgr := setupTestManager(t, []string{ScopeGmailReadonly})
			mgr.config.ClientID = "synthetic-client"
			mgr.config.Endpoint = oauth2.Endpoint{AuthURL: "https://accounts.example/authorize", TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}
			mgr.profileURL = server.URL + "/profile"
			required.NoError(mgr.saveToken(email, &oauth2.Token{AccessToken: "old-access", RefreshToken: "old-refresh", Expiry: time.Now().Add(-time.Hour)}, []string{ScopeGmailReadonly}))
			refreshed := make(chan error, 1)
			go func() {
				if force {
					refreshed <- mgr.ForceRefresh(t.Context(), email)
					return
				}
				_, err := mgr.TokenSource(t.Context(), email)
				refreshed <- err
			}()
			select {
			case <-refreshStarted:
			case <-time.After(10 * time.Second):
				required.FailNow("refresh did not reach the token endpoint")
			}
			authorizer := mgr.withScopes([]string{ScopeCardDAV, ScopeUserinfoEmail})
			flow, err := authorizer.BeginWebAuthorization(email, "https://archive.example/")
			required.NoError(err)
			required.NoError(flow.Complete(t.Context(), flow.State, "synthetic-code"))
			release()
			required.NoError(<-refreshed)
			saved, err := mgr.loadTokenFile(email)
			required.NoError(err)
			assertions.Equal("new-authorization", saved.AccessToken)
			assertions.Equal("new-refresh", saved.RefreshToken)
			assertions.ElementsMatch([]string{ScopeCardDAV, ScopeUserinfoEmail, ScopeGmailReadonly}, saved.Scopes)
		})
	}
}
