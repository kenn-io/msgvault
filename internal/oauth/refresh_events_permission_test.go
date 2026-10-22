package oauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"golang.org/x/oauth2"
)

func TestManagerOmittedRefreshScopeKeepsEventsPermissionUnknown(t *testing.T) {
	for _, command := range []bool{false, true} {
		for _, cleared := range []bool{false, true} {
			t.Run(fmt.Sprintf("command=%v/cleared=%v", command, cleared), func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				mgr := setupTestManager(t, ScopesGmailReadonly)
				if command {
					mgr, _ = commandManager(t)
				}
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					call := calls.Add(1)
					fields := ""
					if cleared && call == 1 {
						fields = `,"scope":""`
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"access_token":"refreshed-%d","token_type":"Bearer","expires_in":3600%s}`, call, fields)
				}))
				t.Cleanup(server.Close)
				mgr.config.Endpoint = oauth2.Endpoint{TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInParams}
				fixture := storetest.New(t)
				email := fixture.Source.Identifier
				var savedScopes []string
				if cleared {
					savedScopes = ScopesGmailReadonly
				}
				require.NoError(mgr.saveToken(email, &oauth2.Token{AccessToken: "expired", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour)}, savedScopes))
				if cleared {
					require.NoError(mgr.ForceRefresh(t.Context(), email))
					require.Empty(mgr.GrantedScopes(t.Context(), email), "the first refresh explicitly clears the saved grant")
				}

				require.NoError(mgr.ForceRefresh(t.Context(), email))
				info, err := mgr.InspectToken(t.Context(), email)
				require.NoError(err)
				assert.Empty(info.Scopes, "omitted scope must preserve unknown metadata instead of granting the configured request scopes")
				assert.False(mgr.HasScopeMetadata(t.Context(), email))
				client := gmail.NewClient(nil, gmail.WithGrantedScopesReader(func() []string {
					return mgr.GrantedScopes(t.Context(), email)
				}))
				read, known := client.NativeMessageReadPermission()
				assert.False(known)
				assert.False(read)

				_, err = fixture.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
				require.NoError(err)
				resource, err := fixture.Store.GetMCPResource(t.Context(), "source", fixture.Source.ID)
				require.NoError(err)
				require.NoError(fixture.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: fixture.Source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: email, Known: known, MessageRead: read}))
				allowed, err := fixture.Store.NativeSourceMessagePermission(t.Context(), fixture.Source.ID)
				require.NoError(err)
				assert.False(allowed, "a refresh without scope evidence cannot authorize Events message reads")
				_, _, err = fixture.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.source_message_archived", "source", fixture.Source.ID, []string{"message"}, resource.Reference)
				require.Error(err, "unknown Gmail permission must reject Events subscription admission")
			})
		}
	}
}
