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

func newIMAPDraftLifecycleFixture(t *testing.T) (*store.Store, *store.Source, int64, store.IMAPDraftReceipt) {
	requirements := require.New(t)

	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "imap://alice@example.com:143")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "draft-thread", "Draft thread")
	requirements.NoError(err)
	receipt := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 101, UID: 1}
	participants := []store.ParticipantPersistData{
		{EmailAddress: "alice@example.com", Domain: "example.com"},
		{EmailAddress: "bob@example.com", Domain: "example.com"},
	}
	raw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Draft\r\nMessage-ID: <draft@example.com>\r\n\r\nDraft body\r\n")
	draftID, err := st.PersistIMAPDraftContext(context.Background(), receipt, participants, func(ids []int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
				ConversationID: conversationID, MessageType: store.MessageTypeEmail,
				SenderID:        sql.NullInt64{Int64: ids[0], Valid: true},
				RFC822MessageID: sql.NullString{String: "<draft@example.com>", Valid: true},
				Subject:         sql.NullString{String: "Draft", Valid: true},
				Snippet:         sql.NullString{String: "Draft body", Valid: true},
				ArchivedAt:      time.Now(), SizeEstimate: int64(len(raw)),
			},
			BodyText: sql.NullString{String: "Draft body", Valid: true},
			RawMIME:  raw,
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: []int64{ids[0]}, EmailAddresses: []string{"alice@example.com"}},
				{Type: "to", ParticipantIDs: []int64{ids[1]}, EmailAddresses: []string{"bob@example.com"}},
			},
		}
	})
	requirements.NoError(err)
	return st, source, draftID, receipt
}

func TestPersistIMAPDraftContextRegistersOwnership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, source, draftID, receipt := newIMAPDraftLifecycleFixture(t)
	var got store.IMAPDraft
	var uidValidity, uid int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT draft_id, source_id, current_message_id, mailbox, uidvalidity, uid,
		       revision, lifecycle
		FROM imap_drafts WHERE draft_id = ?
	`), draftID).Scan(&got.DraftID, &got.SourceID, &got.CurrentMessageID, &got.Mailbox, &uidValidity, &uid, &got.Revision, &got.Lifecycle))

	assertions.Equal(draftID, got.DraftID)
	assertions.Equal(source.ID, got.SourceID)
	assertions.Equal(draftID, got.CurrentMessageID)
	assertions.Equal(receipt.Mailbox, got.Mailbox)
	assertions.Equal(int64(receipt.UIDValidity), uidValidity)
	assertions.Equal(int64(receipt.UID), uid)
	assertions.Equal(int64(1), got.Revision)
	assertions.Equal("active", got.Lifecycle)
}

func TestGetIMAPDraftContextFollowsCurrentMembershipWithoutWriting(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, source, draftID, receipt := newIMAPDraftLifecycleFixture(t)
	_, err := st.DB().Exec(st.Rebind(`
		DELETE FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
	`), source.ID, receipt.Mailbox, receipt.UIDValidity, receipt.UID)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO imap_message_memberships
			(source_id, mailbox, uidvalidity, uid, message_id, flags, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`), source.ID, "Archive", 202, 77, draftID, `["\\Draft"]`)
	requirements.NoError(err)

	draft, err := st.GetIMAPDraftContext(context.Background(), draftID)
	requirements.NoError(err)
	assertions.Equal("Archive", draft.Mailbox)
	assertions.Equal(uint32(202), draft.UIDValidity)
	assertions.Equal(uint32(77), draft.UID)
	var storedMailbox string
	var storedUIDValidity, storedUID int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mailbox, uidvalidity, uid FROM imap_drafts WHERE draft_id = ?
	`), draftID).Scan(&storedMailbox, &storedUIDValidity, &storedUID))
	assertions.Equal(receipt.Mailbox, storedMailbox)
	assertions.Equal(int64(receipt.UIDValidity), storedUIDValidity)
	assertions.Equal(int64(receipt.UID), storedUID)
}

func TestGetIMAPDraftContextReadsSourceDeletedMessage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, _, draftID, _ := newIMAPDraftLifecycleFixture(t)
	_, err := st.DB().Exec(st.Rebind(`
		UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?
	`), draftID)
	requirements.NoError(err)

	draft, err := st.GetIMAPDraftContext(context.Background(), draftID)
	requirements.NoError(err)
	assertions.Equal("Draft", draft.Subject)
	assertions.Equal("Draft body", draft.Snippet)
}

func TestIMAPDraftGCRetainsCurrentMessage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, _, draftID, _ := newIMAPDraftLifecycleFixture(t)
	if st.IsPostgreSQL() {
		t.Skip("GC retention test is SQLite-only")
	}
	_, err := st.DB().Exec(st.Rebind(`
		UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?
	`), draftID)
	requirements.NoError(err)

	plan, err := st.PlanGCContext(context.Background())
	requirements.NoError(err)
	assertions.Zero(plan.SourceDeleted)
	deleted, err := st.ExecuteGCContext(context.Background(), plan)
	requirements.NoError(err)
	assertions.Zero(deleted)

	draft, err := st.GetIMAPDraftContext(context.Background(), draftID)
	requirements.NoError(err)
	assertions.Equal(draftID, draft.CurrentMessageID)
	assertions.Equal("Draft body", draft.Snippet)
}
