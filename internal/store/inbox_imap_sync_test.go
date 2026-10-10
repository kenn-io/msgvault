package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestInboxIMAPSyncUnknownFlagsRefreshAndEpochRetirement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "imap://reader%40example.test@mail.example.test:143")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "native-state", "Native state")
	requirements.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "INBOX|1", MessageType: "email"})
	requirements.NoError(err)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "imap", SourceIdentifier: source.Identifier, AccountID: "reader@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "INBOX|1", Mailbox: "INBOX", UIDValidity: 77, UID: 1}
	apply := func(epoch uint32, flags []string) {
		t.Helper()
		require.NoError(t, st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: epoch, UIDNext: 2}, Reset: true, Memberships: []store.IMAPMembershipObservation{{Mailbox: "INBOX", UIDValidity: epoch, UID: 1, SourceMessageID: "INBOX|1", Flags: flags}}}}))
	}
	apply(77, nil)
	unknown, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(unknown)
	assertions.Nil(unknown.Read, "omitted flags cannot prove unread")
	apply(77, []string{})
	unread, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(unread)
	requirements.NotNil(unread.Read)
	assertions.False(*unread.Read)
	apply(77, []string{})
	refreshed, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(refreshed)
	assertions.True(refreshed.ObservedAt.After(unread.ObservedAt), "unchanged reset entries still prove freshness")
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET is_read = FALSE WHERE id = ?`), mid)
	requirements.NoError(err)
	apply(77, []string{`\Seen`, `\Flagged`, "Todo"})
	read, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(read)
	requirements.NotNil(read.Read)
	assertions.True(*read.Read)
	assertions.Equal([]string{"Todo"}, read.Tags)
	var uiRead bool
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id = ?`), mid).Scan(&uiRead))
	assertions.False(uiRead)
	apply(78, []string{})
	old, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	assertions.Nil(old, "replaced epoch cannot remain an inbox candidate")
	target.UIDValidity = 78
	current, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(current)
	requirements.NoError(st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 78, UIDNext: 2}, VanishedUIDs: []uint32{1}}}))
	vanished, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	assertions.Nil(vanished)
}

func TestInboxIMAPSyncRetiresRekeyedProviderIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "imap://reader%40example.test@mail.example.test:143")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "rekey-state", "Rekey state")
	requirements.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "INBOX|1", MessageType: "email"})
	requirements.NoError(err)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "imap", SourceIdentifier: source.Identifier, AccountID: "reader@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "INBOX|1", Mailbox: "INBOX", UIDValidity: 77, UID: 1}
	apply := func(providerID string) {
		t.Helper()
		require.NoError(t, st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 77, UIDNext: 2}, Reset: true, Memberships: []store.IMAPMembershipObservation{{Mailbox: "INBOX", UIDValidity: 77, UID: 1, SourceMessageID: providerID, Flags: []string{}}}}}))
	}
	apply(target.ProviderID)
	before, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(before)
	rekeyed, err := st.RekeyMessageSourceID(mid, target.ProviderID, "Archive|9")
	requirements.NoError(err)
	requirements.True(rekeyed)
	apply("Archive|9")
	old, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	assertions.Nil(old, "old canonical identity cannot remain an inbox candidate")
	target.ProviderID = "Archive|9"
	current, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(current)
	assertions.Equal(mid, current.Target.ItemID)
	assertions.True(*current.Inbox)
}
