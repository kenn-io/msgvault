package slack

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func activateNativeSlackSourceReceipt(t *testing.T, imp *Importer, sourceID int64, now time.Time) *store.MCPSubscription {
	t.Helper()
	require := require.New(t)
	resource, err := imp.store.GetMCPResource(t.Context(), "source", sourceID)
	require.NoError(err)
	arguments, err := json.Marshal(map[string]any{"source_id": sourceID, "source_ref": resource.Reference})
	require.NoError(err)
	sub := store.MCPSubscription{
		ID: "synthetic-slack-source-receipt", Principal: "synthetic-owner", Name: "msgvault.source_message_archived",
		Arguments: arguments, ScopeKind: "source", ScopeID: sourceID, SourceID: sourceID,
		CallbackURL: "https://receiver.example.test/slack", SecretEnc: []byte("synthetic-encrypted-secret"),
		SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(24 * time.Hour),
	}
	require.NoError(imp.store.BindMCPSubscriptionScope(t.Context(), &sub))
	active, _, err := imp.store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: sub, Now: now})
	require.NoError(err)
	require.NotNil(active)
	return active
}

func TestNativeSlackCredentialRevocationDuringHistoryReplyMembershipOrSearchFetchRetiresReceipts(t *testing.T) {
	for _, fetch := range []string{"history", "replies", "members", "search"} {
		t.Run(fetch, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := nativeEventsWorkspace(t)
			imp, opts := testImporter(t, f)
			enableNativeSlackInboxEvents(t, imp.store)
			now := tsBase.Add(24 * time.Hour)
			imp.now = func() time.Time { return now }

			summary, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			sub := activateNativeSlackSourceReceipt(t, imp, summary.SourceID, now)

			liveTS := tsFormat(now.Add(time.Minute))
			f.convs[0].Msgs = append(f.convs[0].Msgs, fakeMsg{TS: liveTS, User: "USENDER", Text: "Synthetic live Slack message"})
			now = now.Add(time.Hour)
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			var eventCount int
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.source_message_archived'`).Scan(&eventCount))
			require.Positive(eventCount, "the live Slack import should record a source receipt")
			pending, err := imp.store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
				return []byte(`{"eventId":"synthetic-slack-event"}`), nil
			})
			require.NoError(err)
			require.NotNil(pending, "the real Slack importer should create a retained pending delivery")
			_, err = imp.store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now)
			require.NoError(err, "the live source receipt is readable before credential revocation")
			var body string
			require.NoError(imp.store.ReadMCPEventMessage(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, pending.Event.MessageID, now, func(tx *sql.Tx) error {
				return tx.QueryRow(imp.store.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), pending.Event.MessageID).Scan(&body)
			}))
			assert.Equal("Synthetic live Slack message", body)

			switch fetch {
			case "history":
				f.credentialHistory["CEVENTS"] = true
			case "replies":
				rootTS := ts(0)
				f.convs[0].Msgs[0].Replies = append(f.convs[0].Msgs[0].Replies, fakeMsg{TS: tsFormat(now.Add(time.Minute)), ThreadTS: rootTS, User: "USENDER", Text: "Synthetic late Slack reply"})
				f.credentialReplies[rootTS] = true
			case "members":
				f.credentialMembers["CEVENTS"] = true
			case "search":
				f.credentialSearch = true
			}
			now = now.Add(time.Hour)
			_, err = imp.Import(t.Context(), opts)
			assert.ErrorIs(err, ErrCredentialRevoked, "the %s error must reach the import finalizer", fetch)
			allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), summary.SourceID)
			require.NoError(err)
			assert.False(allowed, "the %s credential error must revoke the retained native Events grant", fetch)

			next, err := imp.store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now.Add(time.Second), func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
				return []byte(`{"eventId":"synthetic-slack-event"}`), nil
			})
			require.NoError(err)
			assert.Nil(next, "retained pending delivery cannot pass the revoked source grant")
			_, err = imp.store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now.Add(time.Second))
			assert.Error(err, "the retained source receipt must be inaccessible after account revocation")
			readCalled := false
			err = imp.store.ReadMCPEventMessage(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, pending.Event.MessageID, now.Add(time.Second), func(*sql.Tx) error {
				readCalled = true
				return nil
			})
			assert.Error(err, "revoked receipt cannot authorize archived message content")
			assert.False(readCalled, "revoked receipt must be rejected before message content is read")
		})
	}
}
