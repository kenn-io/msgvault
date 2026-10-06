package imap

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInboxIMAPTriagePreviewFromCommittedSyncPreservesNativeKeywords(t *testing.T) {
	var writes, creates, writable atomic.Int64
	client, id := newKeywordTestClientFor(t, keywordTestSession{permanent: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged, "Todo"}, stores: &writes, creates: &creates, writableSelects: &writable})
	st := testutil.NewTestStore(t)
	archived, err := st.GetOrCreateSource("imap", client.config.Identifier())
	require.NoError(t, err)
	source, target := inboxIMAPBinding(client, id)
	source.SourceID = archived.ID
	target.SourceID = archived.ID
	conversation, err := st.EnsureConversation(source.SourceID, "synthetic-inbox", "Synthetic inbox")
	require.NoError(t, err)
	target.ItemID, err = st.UpsertMessage(&store.Message{SourceID: source.SourceID, ConversationID: conversation, SourceMessageID: target.ProviderID, MessageType: "email"})
	require.NoError(t, err)
	require.NoError(t, st.ApplyIMAPMailboxDeltas(source.SourceID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", Reset: true, State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: id.UIDValidity, UIDNext: id.UID + 1}, Memberships: []store.IMAPMembershipObservation{{Mailbox: "INBOX", UIDValidity: id.UIDValidity, UID: id.UID, SourceMessageID: target.ProviderID, Flags: []string{}}}}}))
	_, err = st.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Todo"}, 0, inboxcontrol.Principal{ID: "owner-fixture", Owner: true})
	require.NoError(t, err)
	service := &inboxcontrol.Service{Ledger: st, Key: []byte(strings.Repeat("k", 32)), Authorize: func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, Resolve: func(ctx context.Context, r inboxcontrol.Request) (inboxcontrol.Provider, error) {
		if r.Target != nil {
			if err := st.ValidateInboxTargetContext(ctx, *r.Target); err != nil {
				return nil, err
			}
		} else if r.Source == nil || *r.Source != source {
			return nil, inboxcontrol.ErrDenied
		}
		return NewInboxProvider(client, source), nil
	}}
	// The native fixture selects once to discover UIDVALIDITY before preview.
	writableBefore := writable.Load()
	proposal, err := service.PreviewTriage(t.Context(), inboxcontrol.TriageInput{Source: source, Items: []inboxcontrol.TriageItemInput{{Target: target, Categories: []string{"todo"}, IdempotencyKey: "imap-triage-1"}}}, inboxcontrol.Principal{ID: "delegate-fixture"})
	require.NoError(t, err)
	require.Len(t, proposal.Items, 1)
	assert.Equal(t, []string{"Todo"}, proposal.Items[0].Request.Tags.Add)
	assert.Contains(t, proposal.Items[0].Projected.Tags, "Todo")
	assert.Contains(t, proposal.Items[0].Projected.Flags, "Todo")
	assert.True(t, *proposal.Items[0].Projected.Inbox)
	assert.False(t, *proposal.Items[0].Projected.Read)
	assert.Zero(t, writes.Load())
	assert.Zero(t, creates.Load())
	assert.Equal(t, writableBefore, writable.Load(), "preview must use read-only selection")
}
