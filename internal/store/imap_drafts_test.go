package store_test

import (
	"context"
	"database/sql"
	"fmt"
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
	source, err := st.GetOrCreateSource("imap", "imap://alice@example.com:143")
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
				RFC822MessageID: sql.NullString{String: fmt.Sprintf("draft-%d@example.com", receipt.UIDValidity), Valid: true},
				Subject:         sql.NullString{String: "Re: Thread", Valid: true},
				ArchivedAt:      time.Now(), SizeEstimate: 10,
			},
			BodyText: sql.NullString{String: "draft body", Valid: true},
			RawMIME:  fmt.Appendf(nil, "From: alice@example.com\r\n\r\n draft body epoch %d\r\n", receipt.UIDValidity),
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: []int64{ids[0]}, EmailAddresses: []string{"alice@example.com"}},
				{Type: "to", ParticipantIDs: []int64{ids[1]}, EmailAddresses: []string{"user@example.com"}},
			},
		}
	}
	participants := []store.ParticipantPersistData{
		{EmailAddress: "alice@example.com", Domain: "example.com"},
		{EmailAddress: "user@example.com", Domain: "example.com"},
	}
	id, err := st.PersistIMAPDraftContext(context.Background(), receipt, participants, build)
	requirements.NoError(err)
	assertions.Positive(id)
	var draftMembershipCount, cursorCount int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE message_id = ?`), id).Scan(&draftMembershipCount))
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM imap_folder_state WHERE source_id = ?`), source.ID).Scan(&cursorCount))
	assertions.Equal(1, draftMembershipCount)
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
	states, err = st.GetIMAPFolderStates(source.ID)
	requirements.NoError(err)
	receipt.UIDValidity++
	// A failed insert must also roll back the old message's rekey.
	_, err = st.PersistIMAPDraftContext(context.Background(), receipt, nil, func([]int64) *store.MessagePersistData { return nil })
	requirements.ErrorContains(err, "persist message requires a message")
	oldSourceID, err := st.GetMessageSourceID(id)
	requirements.NoError(err)
	assertions.Equal(store.IMAPDraftSourceMessageID(receipt), oldSourceID)

	newID, err := st.PersistIMAPDraftContext(context.Background(), receipt, participants, build)
	requirements.NoError(err)
	assertions.NotEqual(id, newID)
	oldRaw, err := st.GetMessageRaw(id)
	requirements.NoError(err)
	assertions.Equal(raw, oldRaw)
	newRaw, err := st.GetMessageRaw(newID)
	requirements.NoError(err)
	assertions.Contains(string(newRaw), "draft body epoch 8")
	oldSourceID, err = st.GetMessageSourceID(id)
	requirements.NoError(err)
	assertions.Equal(fmt.Sprintf("msgvault-invalidated:%d", id), oldSourceID)
	unchangedStates, err := st.GetIMAPFolderStates(source.ID)
	requirements.NoError(err)
	assertions.ElementsMatch(states, unchangedStates)
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT message_id FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
	`), source.ID, receipt.Mailbox, receipt.UIDValidity, receipt.UID).Scan(&reconciledID))
	assertions.Equal(newID, reconciledID)

	// The next sync retires the old epoch without replacing the new draft.
	requirements.NoError(st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{{
		Mailbox: receipt.Mailbox, Reset: true,
		State: store.IMAPFolderState{Mailbox: receipt.Mailbox, UIDValidity: receipt.UIDValidity, UIDNext: receipt.UID + 1},
		Memberships: []store.IMAPMembershipObservation{{
			UID: receipt.UID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt), Flags: []string{"\\Draft"},
		}},
	}}))
	assertions.True(messageTombstoned(t, st, id))
	assertions.False(messageTombstoned(t, st, newID))
	assertions.Equal(1, membershipCount(t, st, source.ID))
	var messageCount int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, store.IMAPDraftSourceMessageID(receipt)).Scan(&messageCount))
	assertions.Equal(1, messageCount)
}

func TestPersistIMAPDraftRekeysOrphanedSourceDeletedMessage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "imap://legacy@example.com:143")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "legacy-draft", "Legacy draft")
	requirements.NoError(err)
	oldID, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: source.ID, SourceMessageID: "Drafts|1",
			ConversationID: conversationID, MessageType: store.MessageTypeEmail,
		},
		BodyText: sql.NullString{String: "old draft", Valid: true},
		RawMIME:  []byte("Subject: Old draft\r\n\r\nold draft\r\n"),
	})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?
	`), oldID)
	requirements.NoError(err)

	receipt := store.IMAPDraftReceipt{
		SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 20, UID: 1,
	}
	newID, err := st.PersistIMAPDraftContext(context.Background(), receipt, nil,
		func([]int64) *store.MessagePersistData {
			return &store.MessagePersistData{
				Message: &store.Message{
					SourceID:        source.ID,
					SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
					ConversationID:  conversationID,
					MessageType:     store.MessageTypeEmail,
				},
				BodyText: sql.NullString{String: "new draft", Valid: true},
				RawMIME:  []byte("Subject: New draft\r\n\r\nnew draft\r\n"),
			}
		})
	requirements.NoError(err)
	assertions.NotEqual(oldID, newID)
	oldSourceMessageID, err := st.GetMessageSourceID(oldID)
	requirements.NoError(err)
	assertions.Equal(fmt.Sprintf("msgvault-invalidated:%d", oldID), oldSourceMessageID)
}
