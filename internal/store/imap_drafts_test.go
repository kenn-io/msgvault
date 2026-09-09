package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersistIMAPDraft(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "imap://alice@host:143")
	requirements.NoError(err)
	initialStates := []store.IMAPFolderState{
		{Mailbox: "INBOX", UIDValidity: 101, UIDNext: 900, HighestModSeq: 1 << 40},
		{Mailbox: "Drafts*2026", UIDValidity: 6, UIDNext: 41, HighestModSeq: 1 << 41},
		{Mailbox: "Archive", UIDValidity: 8, UIDNext: 77, HighestModSeq: 1 << 42},
	}
	requirements.NoError(st.UpsertIMAPFolderStates(source.ID, initialStates))
	conversationID, err := st.EnsureConversation(source.ID, "thread-666", "Thread")
	requirements.NoError(err)
	receipt := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts*2026", UIDValidity: 7, UID: 42}
	build := func(ids []int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
				ConversationID: conversationID, MessageType: store.MessageTypeEmail,
				SenderID:        sql.NullInt64{Int64: ids[0], Valid: true},
				RFC822MessageID: sql.NullString{String: "draft-42@example.com", Valid: true},
				Subject:         sql.NullString{String: "Re: Thread", Valid: true},
				ArchivedAt:      time.Now(), SizeEstimate: 10,
			},
			BodyText: sql.NullString{String: "draft body", Valid: true},
			RawMIME:  []byte("From: alice@example.com\r\n\r\n draft body\r\n"),
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: []int64{ids[0]}, EmailAddresses: []string{"alice@example.com"}},
				{Type: "to", ParticipantIDs: []int64{ids[1]}, EmailAddresses: []string{"user@example.com"}},
			},
		}
	}
	id, err := st.PersistIMAPDraftContext(context.Background(), receipt, []store.ParticipantPersistData{
		{EmailAddress: "alice@example.com", Domain: "example.com"},
		{EmailAddress: "user@example.com", Domain: "example.com"},
	}, build)
	requirements.NoError(err)
	assertions.Positive(id)
	var membershipCount, cursorCount int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE message_id = ?`), id).Scan(&membershipCount))
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM imap_folder_state WHERE source_id = ?`), source.ID).Scan(&cursorCount))
	assertions.Equal(1, membershipCount)
	assertions.Equal(len(initialStates), cursorCount)
	states, err := st.GetIMAPFolderStates(source.ID)
	requirements.NoError(err)
	assertions.ElementsMatch(initialStates, states)
	raw, err := st.GetMessageRaw(id)
	requirements.NoError(err)
	assertions.Contains(string(raw), "draft body")
	requirements.NoError(st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Drafts*2026",
		State:   store.IMAPFolderState{Mailbox: "Drafts*2026", UIDValidity: 7, UIDNext: 43, HighestModSeq: 11},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "Drafts*2026", UIDValidity: 7, UID: 42,
			SourceMessageID: store.IMAPDraftSourceMessageID(receipt), Flags: []string{"\\Draft"},
		}},
	}}))
	var reconciledID int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT message_id FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
	`), source.ID, receipt.Mailbox, receipt.UIDValidity, receipt.UID).Scan(&reconciledID))
	assertions.Equal(id, reconciledID)

	_, err = st.PersistIMAPDraftContext(context.Background(), receipt, nil, build)
	requirements.ErrorContains(err, "source_key_conflict")
	receipt.UIDValidity++
	_, err = st.PersistIMAPDraftContext(context.Background(), receipt, nil, build)
	requirements.ErrorContains(err, "source_key_conflict")
	var messageCount int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, store.IMAPDraftSourceMessageID(receipt)).Scan(&messageCount))
	assertions.Equal(1, messageCount)
}
