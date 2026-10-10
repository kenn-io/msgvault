package msmail

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// This exercises the real controller, durable Store and Graph HTTP client.
// The synthetic Graph endpoint models a category write whose reply can be lost;
// it does not establish production guarantees for Microsoft's If-Match support.
func TestMicrosoftTriageApplyNativePreservationAndReceiptRecovery(t *testing.T) {
	for _, mode := range []string{"verified", "unknown", "deleted-category"} {
		t.Run(mode, func(t *testing.T) {
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
			principal := inboxcontrol.Principal{ID: "delegate-fixture"}
			var writes atomic.Int64
			var deleted atomic.Bool
			var mu sync.Mutex
			tags := []string{"Unrelated"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				assert.Contains(t, r.Header.Get("Prefer"), `IdType="ImmutableId"`)
				switch r.URL.Path {
				case "/me":
					assert.NoError(t, json.MarshalWrite(w, map[string]string{"mail": source.AccountID}))
				case "/me/mailFolders/inbox":
					assert.NoError(t, json.MarshalWrite(w, map[string]string{"id": "inbox-id"}))
				case "/me/outlook/masterCategories":
					catalog := []map[string]string{{"id": "master-1", "displayName": "Todo"}}
					if deleted.Load() {
						catalog = nil
					}
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"value": catalog}))
				case "/me/messages/immutable-1":
					if r.Method == http.MethodPatch {
						writes.Add(1)
						assert.Equal(t, `W/"v1"`, r.Header.Get("If-Match"))
						receipt, err := st.LookupInboxReceipt(r.Context(), principal.ID, source.SourceID, "graph-triage-once")
						if !assert.NoError(t, err) {
							return
						}
						if !assert.NotNil(t, receipt) {
							return
						}
						assert.Equal(t, inboxcontrol.StatusDispatching, receipt.Status)
						lease, err := st.AcquireSyncExecutionContext(r.Context(), source.SourceID)
						assert.Error(t, err)
						if lease != nil {
							assert.NoError(t, lease.Release())
						}
						var body map[string][]string
						if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
							return
						}
						if !assert.Len(t, body, 1) {
							return
						}
						assert.ElementsMatch(t, []string{"Unrelated", "Todo"}, body["categories"])
						tags = body["categories"]
						if mode == "unknown" {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
					} else {
						assert.Equal(t, http.MethodGet, r.Method)
					}
					etag := `W/"v1"`
					if writes.Load() > 0 {
						etag = `W/"v2"`
					}
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"id": "immutable-1", "categories": tags, "isRead": false, "parentFolderId": "inbox-id", "@odata.etag": etag}))
				default:
					assert.Fail(t, "unexpected native Graph request", r.Method+" "+r.URL.Path)
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
			service := &inboxcontrol.Service{Ledger: st, Key: []byte(strings.Repeat("k", 32)), Authorize: func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, Resolve: func(ctx context.Context, r inboxcontrol.Request) (inboxcontrol.Provider, error) {
				if r.Target != nil {
					if err := st.ValidateInboxTargetContext(ctx, *r.Target); err != nil {
						return nil, err
					}
				}
				return provider(), nil
			}, AcquireSource: func(ctx context.Context, id int64) (func(), error) {
				lease, err := st.AcquireSyncExecutionContext(ctx, id)
				if err != nil {
					return nil, err
				}
				return func() { assert.NoError(t, lease.Release()) }, nil
			}, ReconcileState: func(ctx context.Context, before, after inboxcontrol.State) error {
				return st.ReconcileInboxProviderState(ctx, before.Target, before, after)
			}}
			proposal, err := service.PreviewTriage(t.Context(), inboxcontrol.TriageInput{Source: source, Items: []inboxcontrol.TriageItemInput{{Target: target, Categories: []string{"todo"}, IdempotencyKey: "graph-triage-once"}}}, principal)
			requirements.NoError(err)
			assertions.Zero(writes.Load())
			deleted.Store(mode == "deleted-category")
			gateCalls := 0
			results, err := service.ApplyTriage(t.Context(), *proposal, principal, func(context.Context) (func(), error) { gateCalls++; return func() {}, nil })
			assertions.Equal(1, gateCalls)
			if mode == "deleted-category" {
				require.Error(t, err)
				assertions.Zero(writes.Load())
				return
			}
			want := inboxcontrol.StatusVerified
			if mode == "unknown" {
				require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
				want = inboxcontrol.StatusUnknown
			} else {
				requirements.NoError(err)
			}
			requirements.Len(results, 1)
			requirements.NotNil(results[0].Receipt)
			assertions.Equal(want, results[0].Receipt.Status)
			if mode == "verified" {
				requirements.NotNil(results[0].After)
				assertions.True(*results[0].After.Inbox)
				assertions.False(*results[0].After.Read)
				assertions.Equal("inbox-id", results[0].After.Location)
				assertions.ElementsMatch([]string{"Unrelated", "Todo"}, results[0].After.Tags)
			}
			assertions.Equal(int64(1), writes.Load())
			restarted := *service
			restarted.Now = func() time.Time { return proposal.ExpiresAt.Add(time.Hour) }
			restarted.Resolve = func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) {
				assert.Fail(t, "receipt recovery contacted provider")
				return nil, inboxcontrol.ErrUnavailable
			}
			again, err := restarted.ApplyTriage(t.Context(), *proposal, principal, nil)
			if mode == "unknown" {
				require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
			} else {
				requirements.NoError(err)
			}
			requirements.Len(again, 1)
			requirements.NotNil(again[0].Receipt)
			assertions.Equal(results[0].Receipt.ID, again[0].Receipt.ID)
			assertions.Equal(int64(1), writes.Load())
		})
	}
}
