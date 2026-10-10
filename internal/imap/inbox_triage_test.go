package imap

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInboxIMAPTriagePreviewAndApplyFromCommittedSyncPreservesNativeKeywords(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var writes, creates, writable atomic.Int64
	client, id := newKeywordTestClientFor(t, keywordTestSession{permanent: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged, "Todo"}, stores: &writes, creates: &creates, writableSelects: &writable})
	st := testutil.NewTestStore(t)
	archived, err := st.GetOrCreateSource("imap", client.config.Identifier())
	requirements.NoError(err)
	source, target := inboxIMAPBinding(client, id)
	source.SourceID = archived.ID
	target.SourceID = archived.ID
	conversation, err := st.EnsureConversation(source.SourceID, "synthetic-inbox", "Synthetic inbox")
	requirements.NoError(err)
	target.ItemID, err = st.UpsertMessage(&store.Message{SourceID: source.SourceID, ConversationID: conversation, SourceMessageID: target.ProviderID, MessageType: "email"})
	requirements.NoError(err)
	requirements.NoError(st.ApplyIMAPMailboxDeltas(source.SourceID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", Reset: true, State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: id.UIDValidity, UIDNext: id.UID + 1}, Memberships: []store.IMAPMembershipObservation{{Mailbox: "INBOX", UIDValidity: id.UIDValidity, UID: id.UID, SourceMessageID: target.ProviderID, Flags: []string{}}}}}))
	_, err = st.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Todo"}, 0, inboxcontrol.Principal{ID: "owner-fixture", Owner: true})
	requirements.NoError(err)
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
	requirements.NoError(err)
	requirements.Len(proposal.Items, 1)
	assertions.Equal([]string{"Todo"}, proposal.Items[0].Request.Tags.Add)
	assertions.Contains(proposal.Items[0].Projected.Tags, "Todo")
	assertions.Contains(proposal.Items[0].Projected.Flags, "Todo")
	assertions.True(*proposal.Items[0].Projected.Inbox)
	assertions.False(*proposal.Items[0].Projected.Read)
	assertions.Zero(writes.Load())
	assertions.Zero(creates.Load())
	assertions.Equal(writableBefore, writable.Load(), "preview must use read-only selection")
	service.AcquireSource = func(ctx context.Context, id int64) (func(), error) {
		lease, err := st.AcquireSyncExecutionContext(ctx, id)
		if err != nil {
			return nil, err
		}
		return func() { assert.NoError(t, lease.Release()) }, nil
	}
	service.ReconcileState = func(ctx context.Context, before, after inboxcontrol.State) error {
		return st.ReconcileInboxProviderState(ctx, before.Target, before, after)
	}
	results, err := service.ApplyTriage(t.Context(), *proposal, inboxcontrol.Principal{ID: "delegate-fixture"}, func(context.Context) (func(), error) { return func() {}, nil })
	requirements.NoError(err)
	requirements.Len(results, 1)
	requirements.NotNil(results[0].Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, results[0].Receipt.Status)
	assertions.Equal(int64(1), writes.Load())
	assertions.Zero(creates.Load())
	requirements.NotNil(results[0].After)
	assertions.True(*results[0].After.Inbox)
	assertions.False(*results[0].After.Read)
	assertions.True(emailtags.Contains(results[0].After.Flags, "Todo", true))
	// Native resync must retain the added keyword and both inbox/read markers.
	requirements.NoError(st.ApplyIMAPMailboxDeltas(source.SourceID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: id.UIDValidity, UIDNext: id.UID + 1}, Memberships: []store.IMAPMembershipObservation{{Mailbox: "INBOX", UIDValidity: id.UIDValidity, UID: id.UID, SourceMessageID: target.ProviderID, Flags: results[0].After.Flags}}}}))
	observed, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(observed)
	assertions.True(*observed.Inbox)
	assertions.False(*observed.Read)
	assertions.True(emailtags.Contains(observed.Tags, "Todo", true))
}
