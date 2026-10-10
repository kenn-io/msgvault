package msmail

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMicrosoftTriagePreviewRequiresCurrentNativeCategory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
		want   error
	}{
		{"existing", `{"value":[{"id":"master-1","displayName":"Todo"}]}`, 200, nil},
		{"deleted", `{"value":[]}`, 200, inboxcontrol.ErrDenied},
		{"catalog permission missing", `{}`, 403, inboxcontrol.ErrDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := testutil.NewTestStore(t)
			archived, err := st.GetOrCreateSource("msmail", "mailbox@example.test")
			requirements.NoError(err)
			conversation, err := st.EnsureConversation(archived.ID, "synthetic-thread", "Synthetic thread")
			requirements.NoError(err)
			mid, err := st.UpsertMessage(&store.Message{SourceID: archived.ID, ConversationID: conversation, SourceMessageID: "immutable-1", MessageType: "email"})
			requirements.NoError(err)
			source := inboxcontrol.SourceIdentity{SourceID: archived.ID, SourceType: "msmail", SourceIdentifier: archived.Identifier, AccountID: archived.Identifier}
			target := inboxcontrol.Target{SourceID: source.SourceID, SourceType: source.SourceType, SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "immutable-1"}
			var writes, catalogs atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
					http.Error(w, "write forbidden", http.StatusMethodNotAllowed)
					return
				}
				switch r.URL.Path {
				case "/me":
					assert.NoError(t, json.MarshalWrite(w, map[string]string{"mail": source.AccountID, "userPrincipalName": source.AccountID}))
				case "/me/mailFolders/inbox":
					assert.NoError(t, json.MarshalWrite(w, map[string]string{"id": "inbox-id"}))
				case "/me/messages/immutable-1":
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"id": "immutable-1", "categories": []string{"Unrelated"}, "isRead": false, "parentFolderId": "inbox-id", "@odata.etag": `W/"v1"`}))
				case "/me/outlook/masterCategories":
					catalogs.Add(1)
					w.WriteHeader(tc.status)
					_, err := w.Write([]byte(tc.body))
					assert.NoError(t, err)
				default:
					assert.Fail(t, "unexpected native Graph lookup", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			provider := func() *InboxProvider {
				return NewInboxProvider(NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000), source).WithWriteCapability(inboxcontrol.CapabilitySupported)
			}
			before, err := provider().Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
			requirements.NoError(err)
			_, err = st.ObserveInboxState(t.Context(), before)
			requirements.NoError(err)
			_, err = st.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Todo"}, 0, inboxcontrol.Principal{ID: "owner-fixture", Owner: true})
			requirements.NoError(err)
			service := &inboxcontrol.Service{Ledger: st, Key: []byte(strings.Repeat("k", 32)), Authorize: func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, Resolve: func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) { return provider(), nil }}
			proposal, err := service.PreviewTriage(t.Context(), inboxcontrol.TriageInput{Source: source, Items: []inboxcontrol.TriageItemInput{{Target: target, Categories: []string{"todo"}}}}, inboxcontrol.Principal{ID: "delegate-fixture"})
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assertions.Nil(proposal)
			} else {
				requirements.NoError(err)
				requirements.Len(proposal.Items, 1)
				assertions.Equal([]string{"Todo"}, proposal.Items[0].Request.Tags.Add)
				assertions.True(*proposal.Items[0].Projected.Inbox)
				assertions.False(*proposal.Items[0].Projected.Read)
				assertions.Equal("inbox-id", proposal.Items[0].Projected.Location)
				assertions.Contains(proposal.Items[0].Projected.Tags, "Unrelated")
			}
			assertions.Positive(catalogs.Load())
			assertions.Zero(writes.Load())
		})
	}
}
