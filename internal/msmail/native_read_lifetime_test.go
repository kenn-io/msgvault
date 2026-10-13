package msmail

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Immutable-message lookup must not assign a fetched snapshot to a folder
// lifetime that replaced the one admitted before the native content request.
func TestNativeMicrosoftReadCannotBorrowReplacementFolder(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(strconv.FormatBool(replace), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			enableNativeMailEvents(t, st)
			graph := newFakeGraph(t)
			var blocked atomic.Bool
			release, entered := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			graph.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/me" {
					graph.writeJSON(w, map[string]any{"id": "synthetic-account", "mail": "me@example.com", "userPrincipalName": "me@example.com"})
					return
				}
				if r.URL.Path == "/me/messages/synthetic-stale/$value" && blocked.CompareAndSwap(true, false) {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				graph.serve(w, r)
			})
			c := NewClient(graph.srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000, WithGrantedScopesReader(func() ([]string, error) { return []string{"Mail.Read"}, nil }))
			_, err := Import(t.Context(), st, c, Options{Email: "me@example.com"}, slog.Default())
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
			require.NoError(err)
			require.NotNil(source)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			before, found, err := st.LookupNativeMessageScope(t.Context(), source.ID, resource.Reference, "msmail_folder", "inbox")
			require.NoError(err)
			require.True(found)
			graph.put("synthetic-stale", "inbox")
			blocked.Store(true)
			done := make(chan error, 1)
			go func() {
				_, err := Import(t.Context(), st, c, Options{Email: "me@example.com"}, slog.Default())
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				unblock()
				require.FailNow("native MIME request did not start")
			}
			observation := store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: SourceType, NativeIdentity: source.Identifier, Kind: "msmail_folder", RetiredProviderIDs: []string{"inbox"}}
			retireErr := st.ObserveNativeMessageScopes(t.Context(), observation)
			var replaceErr error
			if replace && retireErr == nil {
				observation.RetiredProviderIDs = nil
				observation.Scopes = []store.NativeMessageScope{{ProviderID: "inbox", DisplayName: "Synthetic replacement folder"}}
				replaceErr = st.ObserveNativeMessageScopes(t.Context(), observation)
			}
			unblock()
			require.NoError(retireErr)
			require.NoError(replaceErr)
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				require.FailNow("native mail importer did not finish")
			}
			current, found, err := st.LookupNativeMessageScope(t.Context(), source.ID, resource.Reference, "msmail_folder", "inbox")
			require.NoError(err)
			require.True(found)
			assert.Equal(replace, current.Present)
			if replace {
				assert.NotEqual(before.Identity.Generation, current.Identity.Generation)
			}
			var count int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Zero(count)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='synthetic-stale'`).Scan(&count))
			assert.Zero(count, "mandatory stale membership must reject the message snapshot")
		})
	}
}
