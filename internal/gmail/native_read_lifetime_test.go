package gmail_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

// Label IDs parsed from real Gmail responses cannot acquire a replacement
// lifetime while their native message request is in flight.
func TestNativeGmailReadCannotBorrowReplacementLabel(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(strconv.FormatBool(replace), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
				{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}},
				{Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"}},
				{Family: "msgvault.inbox_message_archived", SourceType: "gmail", Kinds: []string{"message"}},
			}})
			require.NoError(err)
			var phase atomic.Int64
			release, entered := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				n := phase.Load()
				cursor := strconv.FormatInt(10+n*10, 10)
				var value any
				switch {
				case strings.HasSuffix(r.URL.Path, "/profile"):
					value = map[string]any{"emailAddress": "owner@example.net", "historyId": cursor, "messagesTotal": 1 + n, "threadsTotal": 1 + n}
				case strings.HasSuffix(r.URL.Path, "/labels"):
					value = map[string]any{"labels": []any{map[string]any{"id": "INBOX", "name": "Inbox", "type": "system"}}}
				case strings.HasSuffix(r.URL.Path, "/messages"):
					value = map[string]any{"messages": []any{map[string]any{"id": "old", "threadId": "old-thread"}}, "resultSizeEstimate": 1}
				case strings.HasSuffix(r.URL.Path, "/history"):
					value = map[string]any{"historyId": cursor, "history": []any{map[string]any{"id": "19", "messagesAdded": []any{map[string]any{"message": map[string]any{"id": "live", "threadId": "live-thread", "labelIds": []string{"INBOX"}}}}}}}
				case strings.Contains(r.URL.Path, "/messages/"):
					id := filepath.Base(r.URL.Path)
					if id == "live" {
						close(entered)
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					raw := fmt.Sprintf("From: Synthetic Peer <peer@example.net>\r\nTo: Synthetic Owner <owner@example.net>\r\nSubject: Synthetic lifetime\r\nMessage-ID: <%s@example.net>\r\nDate: Fri, 09 Oct 2026 12:00:00 +0000\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nSynthetic body.\r\n", id)
					value = map[string]any{"id": id, "threadId": id + "-thread", "historyId": cursor, "labelIds": []string{"INBOX"}, "internalDate": "1791547200000", "raw": base64.RawURLEncoding.EncodeToString([]byte(raw))}
				default:
					http.NotFound(w, r)
					return
				}
				assert.NoError(json.NewEncoder(w).Encode(value))
			}))
			t.Cleanup(provider.Close)
			native := gmail.NewNativePermissionFixtureClient(provider.URL, func() []string { return []string{oauth.ScopeGmailReadonly} })
			t.Cleanup(func() { assert.NoError(native.Close()) })
			opts := msgsync.DefaultOptions()
			opts.NoResume = true
			worker := msgsync.New(native, st, opts)
			_, err = worker.Full(t.Context(), "owner@example.net")
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier("gmail", "owner@example.net")
			require.NoError(err)
			require.NotNil(source)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			phase.Store(1)
			done := make(chan error, 1)
			go func() { _, err := worker.Incremental(t.Context(), source); done <- err }()
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				unblock()
				require.FailNow("native message request did not start")
			}
			observation := store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Kind: "gmail_label", RetiredProviderIDs: []string{"INBOX"}}
			retireErr := st.ObserveNativeMessageScopes(t.Context(), observation)
			var replaceErr error
			if replace && retireErr == nil {
				observation.RetiredProviderIDs = nil
				observation.Scopes = []store.NativeMessageScope{{ProviderID: "INBOX", DisplayName: "Synthetic replacement label"}}
				replaceErr = st.ObserveNativeMessageScopes(t.Context(), observation)
			}
			unblock()
			require.NoError(retireErr)
			require.NoError(replaceErr)
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				require.FailNow("native Gmail sync did not finish")
			}
			var count int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Zero(count)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='live'`).Scan(&count))
			assert.Zero(count, "stale label membership rejects the whole message snapshot")
		})
	}
}
