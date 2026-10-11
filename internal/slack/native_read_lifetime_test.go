package slack

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// A real replies response cannot restore a revoked root or borrow a later
// root/channel lifetime even when their provider-local IDs are unchanged.
func TestNativeSlackReadPinsScopeBeforeProviderWork(t *testing.T) {
	for _, kind := range []string{"slack_channel", "slack_thread"} {
		for _, replace := range []bool{false, true} {
			t.Run(kind+"/"+strconv.FormatBool(replace), func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				f := nativeSlackInboxWorkspace(t)
				imp, opts := testImporter(t, f)
				now := tsBase.Add(24 * time.Hour)
				imp.now = func() time.Time { return now }
				enableNativeSlackInboxEvents(t, imp.store)
				sum, err := imp.Import(t.Context(), opts)
				require.NoError(err)
				source, err := imp.store.GetMCPResource(t.Context(), "source", sum.SourceID)
				require.NoError(err)
				providerID, parent := "CEVENTS", ""
				if kind == "slack_thread" {
					providerID, parent = sourceMessageID("CEVENTS", ts(0)), "CEVENTS"
				}
				release, entered := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				original := f.serve()
				var blocked atomic.Bool
				blocked.Store(true)
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/conversations.replies" && r.FormValue("channel") == "CEVENTS" && r.FormValue("ts") == ts(0) && blocked.CompareAndSwap(true, false) {
						close(entered)
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					original.Config.Handler.ServeHTTP(w, r)
				}))
				t.Cleanup(proxy.Close)
				imp.client.baseURL = proxy.URL
				liveTS := strconv.FormatInt(now.Add(time.Minute).Unix(), 10) + ".000100"
				f.convs[0].Msgs[0].Replies = append(f.convs[0].Msgs[0].Replies, fakeMsg{TS: liveTS, ThreadTS: ts(0), User: "USENDER", Text: "Synthetic pre-revocation reply"})
				now = now.Add(time.Hour)
				done := make(chan error, 1)
				go func() { _, err := imp.Import(t.Context(), opts); done <- err }()
				select {
				case <-entered:
				case <-time.After(15 * time.Second):
					unblock()
					require.FailNow("native replies request did not start")
				}
				observation := store.NativeMessageScopeInventory{SourceID: sum.SourceID, SourceReference: source.Reference, SourceType: "slack", NativeIdentity: opts.TeamID + ":" + opts.UserID, Kind: kind, ParentProviderID: parent, RetiredProviderIDs: []string{providerID}}
				retireErr := imp.store.ObserveNativeMessageScopes(t.Context(), observation)
				var replaceErr error
				if replace && retireErr == nil {
					observation.RetiredProviderIDs = nil
					observation.Scopes = []store.NativeMessageScope{{ProviderID: providerID, DisplayName: "Synthetic replacement"}}
					if kind == "slack_channel" {
						observation.Scopes[0].PermissionDomain = "channels"
					}
					replaceErr = imp.store.ObserveNativeMessageScopes(t.Context(), observation)
				}
				unblock()
				require.NoError(retireErr)
				require.NoError(replaceErr)
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					require.FailNow("native importer did not finish")
				}
				var count int
				require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
				assert.Zero(count)
				require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id=?`), sourceMessageID("CEVENTS", liveTS)).Scan(&count))
				assert.Zero(count, "stale scope lifetime rejects the whole message snapshot")
				current, found, err := imp.store.LookupNativeMessageScope(t.Context(), sum.SourceID, source.Reference, kind, providerID)
				require.NoError(err)
				require.True(found)
				assert.Equal(replace, current.Present)
			})
		}
	}
}
