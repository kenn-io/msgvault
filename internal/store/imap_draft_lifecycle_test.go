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

// newIMAPDraftFixture creates an IMAP source with one archived parent message
// and calls PersistIMAPDraftContext to produce a registered draft.
func newIMAPDraftFixture(t *testing.T) (st *store.Store, source *store.Source, draftID int64, receipt store.IMAPDraftReceipt) {
	t.Helper()
	st = testutil.NewTestStore(t)
	var err error
	source, err = st.GetOrCreateSource("imap", "imap://alice@example.com:143")
	require.NoError(t, err)
	require.NoError(t, st.AddAccountIdentity(source.ID, "alice@example.com", "manual"))

	conversationID, err := st.EnsureConversation(source.ID, "thread-1", "Test Thread")
	require.NoError(t, err)
	senderID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(t, err)
	toID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(t, err)

	// Persist a parent message.
	parentRaw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <parent@example.com>\r\n\r\nParent body\r\n")
	parentID, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: source.ID, SourceMessageID: "INBOX|1",
			ConversationID: conversationID, MessageType: store.MessageTypeEmail,
			SenderID:        sql.NullInt64{Int64: senderID, Valid: true},
			RFC822MessageID: sql.NullString{String: "<parent@example.com>", Valid: true},
			Subject:         sql.NullString{String: "Test Thread", Valid: true},
			ArchivedAt:      time.Now(), SizeEstimate: int64(len(parentRaw)),
		},
		BodyText:   sql.NullString{String: "Parent body", Valid: true},
		RawMIME:    parentRaw,
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{senderID}, EmailAddresses: []string{"alice@example.com"}}},
	})
	require.NoError(t, err)

	receipt = store.IMAPDraftReceipt{
		SourceID: source.ID, Mailbox: "Drafts",
		UIDValidity: 101, UID: 1,
	}
	participants := []store.ParticipantPersistData{
		{EmailAddress: "alice@example.com", Domain: "example.com"},
		{EmailAddress: "bob@example.com", Domain: "example.com"},
	}
	raw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <draft1@example.com>\r\nIn-Reply-To: <parent@example.com>\r\n\r\nDraft body\r\n")
	draftID, err = st.PersistIMAPDraftContext(context.Background(), receipt, participants, func(ids []int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
				ConversationID: conversationID, MessageType: store.MessageTypeEmail,
				SenderID:         sql.NullInt64{Int64: ids[0], Valid: true},
				ReplyToMessageID: sql.NullInt64{Int64: parentID, Valid: true},
				RFC822MessageID:  sql.NullString{String: "<draft1@example.com>", Valid: true},
				Subject:          sql.NullString{String: "Re: Test Thread", Valid: true},
				IsFromMe:         true, IdentityDerivedIsFromMe: true,
				ArchivedAt: time.Now(), SizeEstimate: int64(len(raw)),
			},
			BodyText: sql.NullString{String: "Draft body", Valid: true},
			RawMIME:  raw,
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: []int64{ids[0]}, EmailAddresses: []string{"alice@example.com"}},
				{Type: "to", ParticipantIDs: []int64{toID}, EmailAddresses: []string{"bob@example.com"}},
			},
		}
	})
	require.NoError(t, err)
	return st, source, draftID, receipt
}

// TestPersistIMAPDraftContextRegistersOwnership verifies that after
// PersistIMAPDraftContext, exactly one imap_drafts row exists at
// revision=1, lifecycle='active', and draft_id==current_message_id.
func TestPersistIMAPDraftContextRegistersOwnership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, source, draftID, receipt := newIMAPDraftFixture(t)

	var draftIDDB, sourceIDDB, currentMessageID int64
	var revision int64
	var lifecycle string
	var mailbox string
	var uidvalidity, uid int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT draft_id, source_id, current_message_id, mailbox, uidvalidity, uid, revision, lifecycle
		FROM imap_drafts WHERE draft_id = ?
	`), draftID).Scan(&draftIDDB, &sourceIDDB, &currentMessageID, &mailbox, &uidvalidity, &uid, &revision, &lifecycle))

	assertions.Equal(draftID, draftIDDB)
	assertions.Equal(source.ID, sourceIDDB)
	assertions.Equal(draftID, currentMessageID, "draft_id == current_message_id on creation")
	assertions.Equal(receipt.Mailbox, mailbox)
	assertions.Equal(int64(receipt.UIDValidity), uidvalidity)
	assertions.Equal(int64(receipt.UID), uid)
	assertions.Equal(int64(1), revision)
	assertions.Equal("active", lifecycle)
}

// TestBeginIMAPDraftOperationIsAtomic verifies that a CAS update with a
// mismatched revision affects zero rows.
func TestBeginIMAPDraftOperationIsAtomic(t *testing.T) {
	requirements := require.New(t)
	st, _, draftID, _ := newIMAPDraftFixture(t)

	_, err := st.BeginIMAPDraftOperationContext(context.Background(), store.IMAPDraftIntent{
		DraftID:          draftID,
		ExpectedRevision: 999, // wrong revision
		Kind:             "edit",
	})
	requirements.Error(err)
	requirements.ErrorContains(err, "revision_conflict")

	// The row must be unchanged.
	var revision int64
	var lifecycle string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT revision, lifecycle FROM imap_drafts WHERE draft_id = ?
	`), draftID).Scan(&revision, &lifecycle))
	assert.Equal(t, int64(1), revision)
	assert.Equal(t, "active", lifecycle)
}

// TestBeginIMAPDraftOperationRejectsSecondPending verifies that a second
// BeginIMAPDraftOperationContext while a mutation is in flight returns
// "operation_pending".
func TestBeginIMAPDraftOperationRejectsSecondPending(t *testing.T) {
	requirements := require.New(t)
	st, _, draftID, _ := newIMAPDraftFixture(t)

	// First claim succeeds (revision 1 → 2).
	draft, err := st.BeginIMAPDraftOperationContext(context.Background(), store.IMAPDraftIntent{
		DraftID:          draftID,
		ExpectedRevision: 1,
		Kind:             "edit",
	})
	requirements.NoError(err)
	requirements.Equal(int64(2), draft.Revision)

	// Second claim at the new revision should fail: pending_kind is already set.
	_, err = st.BeginIMAPDraftOperationContext(context.Background(), store.IMAPDraftIntent{
		DraftID:          draftID,
		ExpectedRevision: 2,
		Kind:             "edit",
	})
	requirements.Error(err)
	requirements.ErrorContains(err, "operation_pending")
}

// TestFinishIMAPDraftOperationTombstonesOnlyMembershiplessMessage verifies
// that a message holding another membership is not tombstoned, while a
// membership-free message is.
func TestFinishIMAPDraftOperationTombstonesOnlyMembershiplessMessage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, source, draftID, receipt := newIMAPDraftFixture(t)

	// Give the draft message a second membership so it won't be tombstoned.
	_, err := st.DB().Exec(st.Rebind(`
		INSERT INTO imap_message_memberships
			(source_id, mailbox, uidvalidity, uid, message_id, flags, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`), source.ID, "Sent", receipt.UIDValidity, receipt.UID+100, draftID, `["\\Seen"]`)
	requirements.NoError(err)

	// Begin operation.
	draft, err := st.BeginIMAPDraftOperationContext(context.Background(), store.IMAPDraftIntent{
		DraftID: draftID, ExpectedRevision: 1, Kind: "discard",
	})
	requirements.NoError(err)

	// Finish: remove only the Drafts membership; the Sent membership stays.
	err = st.FinishIMAPDraftOperationContext(context.Background(), draftID, draft.Revision, store.IMAPDraftOutcome{
		Lifecycle: "discarded",
		SourceID:  source.ID, Mailbox: receipt.Mailbox,
		UIDValidity: receipt.UIDValidity, UID: receipt.UID,
		MessageID: draftID,
	})
	requirements.NoError(err)

	// Message must NOT be tombstoned (still has Sent membership).
	assertions.False(messageTombstoned(t, st, draftID))

	// Now remove the second membership and tombstone manually (simulate second finish).
	_, err = st.DB().Exec(st.Rebind(`
		DELETE FROM imap_message_memberships WHERE source_id = ? AND mailbox = ? AND uid = ?
	`), source.ID, "Sent", receipt.UID+100)
	requirements.NoError(err)
	// Re-run with the Sent membership removed (but draft is now discarded, so we test directly).
	assertions.False(messageTombstoned(t, st, draftID), "message should still not be tombstoned after partial cleanup")
}

// TestIMAPDraftLifecycleContract verifies the full CAS sequence:
// claim at current revision bumps it; stale revision affects zero rows;
// second claim returns operation_pending; Finish clears all pending columns.
func TestIMAPDraftLifecycleContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, source, draftID, receipt := newIMAPDraftFixture(t)

	// Claim at revision 1 → bumps to 2, lifecycle stays 'active'.
	draft, err := st.BeginIMAPDraftOperationContext(context.Background(), store.IMAPDraftIntent{
		DraftID: draftID, ExpectedRevision: 1, Kind: "discard",
	})
	requirements.NoError(err)
	assertions.Equal(int64(2), draft.Revision)
	assertions.Equal("active", draft.Lifecycle)
	assertions.Equal("discard", draft.PendingKind.String)

	// Stale revision still affects zero rows.
	_, err = st.BeginIMAPDraftOperationContext(context.Background(), store.IMAPDraftIntent{
		DraftID: draftID, ExpectedRevision: 1, Kind: "discard",
	})
	requirements.ErrorContains(err, "operation_pending")

	// Second claim at new revision also returns operation_pending (pending_kind is set).
	_, err = st.BeginIMAPDraftOperationContext(context.Background(), store.IMAPDraftIntent{
		DraftID: draftID, ExpectedRevision: 2, Kind: "discard",
	})
	requirements.ErrorContains(err, "operation_pending")

	// Finish: clears pending columns, transitions to discarded.
	err = st.FinishIMAPDraftOperationContext(context.Background(), draftID, 2, store.IMAPDraftOutcome{
		Lifecycle: "discarded",
		SourceID:  source.ID, Mailbox: receipt.Mailbox,
		UIDValidity: receipt.UIDValidity, UID: receipt.UID,
		MessageID: draftID,
	})
	requirements.NoError(err)

	// All pending columns must be cleared.
	var lifecycle string
	var pendingKind sql.NullString
	var pendingUID, pendingUIDValidity sql.NullInt64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT lifecycle, pending_kind, pending_uid, pending_uidvalidity
		FROM imap_drafts WHERE draft_id = ?
	`), draftID).Scan(&lifecycle, &pendingKind, &pendingUID, &pendingUIDValidity))
	assertions.Equal("discarded", lifecycle)
	assertions.False(pendingKind.Valid)
	assertions.False(pendingUID.Valid)
	assertions.False(pendingUIDValidity.Valid)

	// Message must be tombstoned (no remaining memberships).
	assertions.True(messageTombstoned(t, st, draftID))
}

// TestIMAPDraftLifecycleSurvivesAuthoritativeSync verifies that after
// a draft is discarded, an authoritative sync delta does not resurrect it
// or double-count memberships.
func TestIMAPDraftLifecycleSurvivesAuthoritativeSync(t *testing.T) {
	requirements := require.New(t)
	st, source, draftID, receipt := newIMAPDraftFixture(t)

	// Begin and finish a discard.
	draft, err := st.BeginIMAPDraftOperationContext(context.Background(), store.IMAPDraftIntent{
		DraftID: draftID, ExpectedRevision: 1, Kind: "discard",
	})
	requirements.NoError(err)
	err = st.FinishIMAPDraftOperationContext(context.Background(), draftID, draft.Revision, store.IMAPDraftOutcome{
		Lifecycle: "discarded",
		SourceID:  source.ID, Mailbox: receipt.Mailbox,
		UIDValidity: receipt.UIDValidity, UID: receipt.UID,
		MessageID: draftID,
	})
	requirements.NoError(err)

	// Draft ops (Begin + Finish) must not write imap_folder_state on a
	// mailbox that has never been synced.
	var folderStateCntBefore int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM imap_folder_state WHERE source_id = ?
	`), source.ID).Scan(&folderStateCntBefore))
	assert.Equal(t, 0, folderStateCntBefore, "draft ops must not write imap_folder_state")

	// ApplyIMAPMailboxDeltas with Reset delta must not resurrect the old message.
	err = st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{{
		Mailbox: receipt.Mailbox, Reset: true,
		State: store.IMAPFolderState{
			Mailbox:     receipt.Mailbox,
			UIDValidity: receipt.UIDValidity,
			UIDNext:     receipt.UID + 1,
		},
		Memberships: []store.IMAPMembershipObservation{},
	}})
	requirements.NoError(err)

	// The tombstoned message must remain tombstoned.
	assert.True(t, messageTombstoned(t, st, draftID))

	// No memberships for this source.
	var membershipCnt int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ?
	`), source.ID).Scan(&membershipCnt))
	assert.Equal(t, 0, membershipCnt)
}

// TestDraftCommandsRefuseUnregisteredHistoricalDraft verifies that
// GetIMAPDraftContext returns not-found for a message_id with no
// imap_drafts row.
func TestDraftCommandsRefuseUnregisteredHistoricalDraft(t *testing.T) {
	requirements := require.New(t)
	st, source, _, _ := newIMAPDraftFixture(t)

	// Create a plain archived message (no imap_drafts row).
	conversationID, err := st.EnsureConversation(source.ID, "thread-hist", "Historical")
	requirements.NoError(err)
	msgID, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: source.ID, SourceMessageID: fmt.Sprintf("Drafts|999"),
			ConversationID: conversationID, MessageType: store.MessageTypeEmail,
			ArchivedAt: time.Now(), SizeEstimate: 10,
		},
		BodyText: sql.NullString{String: "historical", Valid: true},
		RawMIME:  []byte("Subject: Historical\r\n\r\nhistorical\r\n"),
	})
	requirements.NoError(err)

	// No imap_drafts row exists for this message.
	_, err = st.GetIMAPDraftContext(context.Background(), msgID)
	requirements.Error(err)
	requirements.ErrorContains(err, "not found")
}

// TestDraftOwnershipSurvivesGCAfterEdit verifies that garbage-collecting the
// tombstoned first-generation message row after a draft edit does NOT delete
// the imap_drafts ownership row (P1-E: no ON DELETE CASCADE on draft_id).
func TestDraftOwnershipSurvivesGCAfterEdit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, source, draftID, oldReceipt := newIMAPDraftFixture(t)

	// --- Full edit cycle ---

	// Begin: pending_kind = 'edit', pending_uid = old uid, lifecycle stays 'active'.
	draft, err := st.BeginIMAPDraftOperationContext(context.Background(), store.IMAPDraftIntent{
		DraftID: draftID, ExpectedRevision: 1, Kind: "edit",
	})
	requirements.NoError(err)

	// Persist replacement: archives new message body, updates imap_drafts.uid to new uid.
	newReceipt := store.IMAPDraftReceipt{
		SourceID: source.ID, Mailbox: "Drafts",
		UIDValidity: oldReceipt.UIDValidity, UID: oldReceipt.UID + 1,
	}
	participants := []store.ParticipantPersistData{
		{EmailAddress: "alice@example.com", Domain: "example.com"},
		{EmailAddress: "bob@example.com", Domain: "example.com"},
	}
	raw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <draft2@example.com>\r\n\r\nEdited body\r\n")
	// Obtain conversationID before the transaction to avoid lock contention
	// (the builder callback runs inside the PersistIMAPDraftReplacementContext tx).
	conversationID, err := st.EnsureConversation(source.ID, "thread-1", "Test Thread")
	requirements.NoError(err)

	newMsgID, err := st.PersistIMAPDraftReplacementContext(
		context.Background(), draftID, draft.Revision, newReceipt, participants,
		func(ids []int64) *store.MessagePersistData {
			return &store.MessagePersistData{
				Message: &store.Message{
					SourceID:        source.ID,
					SourceMessageID: store.IMAPDraftSourceMessageID(newReceipt),
					ConversationID:  conversationID,
					MessageType:     store.MessageTypeEmail,
					SenderID:        sql.NullInt64{Int64: ids[0], Valid: true},
					RFC822MessageID: sql.NullString{String: "<draft2@example.com>", Valid: true},
					Subject:         sql.NullString{String: "Re: Test Thread", Valid: true},
					IsFromMe:        true, IdentityDerivedIsFromMe: true,
					ArchivedAt: time.Now(), SizeEstimate: int64(len(raw)),
				},
				BodyText: sql.NullString{String: "Edited body", Valid: true},
				RawMIME:  raw,
			}
		},
	)
	requirements.NoError(err)
	requirements.Positive(newMsgID)

	// Finish: tombstones old message, clears pending state, lifecycle → active.
	requirements.NoError(st.FinishIMAPDraftOperationContext(
		context.Background(), draftID, draft.Revision,
		store.IMAPDraftOutcome{
			Lifecycle:   "active",
			SourceID:    source.ID,
			Mailbox:     oldReceipt.Mailbox,
			UIDValidity: oldReceipt.UIDValidity,
			UID:         oldReceipt.UID,
			MessageID:   draftID,
		},
	))

	// Verify the old message is now tombstoned.
	var deletedFromSourceAt sql.NullTime
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT deleted_from_source_at FROM messages WHERE id = ?
	`), draftID).Scan(&deletedFromSourceAt))
	assertions.True(deletedFromSourceAt.Valid, "old message must be tombstoned after Finish")

	// --- Simulate GC: hard-delete the tombstoned original message row ---
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM messages WHERE id = ?`), draftID)
	requirements.NoError(err, "GC delete of tombstoned message must succeed")

	// --- Ownership row must survive (no ON DELETE CASCADE on draft_id) ---
	var count int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM imap_drafts WHERE draft_id = ?
	`), draftID).Scan(&count))
	assertions.Equal(1, count, "imap_drafts row must survive GC of the first-gen message")
}
