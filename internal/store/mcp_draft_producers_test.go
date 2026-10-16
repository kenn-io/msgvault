package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func draftProducerFixture(t *testing.T, kind string) *storetest.Fixture {
	t.Helper()
	require := Require.New(t)

	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource(kind, kind+"@example.test")
	require.NoError(err)
	conversationType := "chat"
	if kind == "gmail" || kind == "imap" {
		conversationType = "email_thread"
	}
	conv, err := f.Store.EnsureConversationWithType(source.ID, "draft-conversation", conversationType, "Synthetic draft conversation")
	require.NoError(err)
	f.Source, f.ConvID = source, conv
	enableProducerEvents(t, f.Store, store.MCPEventCapability{Family: "msgvault.draft_changed", SourceType: kind, Kinds: []string{"created", "updated", "deleted"}})
	return f
}

func draftMessageBuild(f *storetest.Fixture, providerID, body string) func([]int64) *store.MessagePersistData {
	return func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{Message: &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: providerID, MessageType: "email", IsFromMe: true}, BodyText: sql.NullString{String: body, Valid: true}, RawMIME: []byte(body)}
	}
}

func assertDraftTransitions(t *testing.T, f *storetest.Fixture, kind, draftID string) {
	t.Helper()
	require := Require.New(t)
	assert := Assert.New(t)

	events := readProducerEvents(t, f.Store, "msgvault.draft_changed")
	require.Len(events, 3)
	for index, event := range events {
		assert.Equal([]string{"created", "updated", "deleted"}[index], event.kind)
		assert.Equal(f.ConvID, event.conversationID)
		assert.Equal(f.Source.ID, event.sourceID)
		assert.Equal(kind, event.data["draft_kind"])
		assert.Equal(draftID, event.data["draft_id"])
		assert.Equal(int64(index+1), producerPayloadInt64(t, event, "revision"))
		assert.Equal("unknown", event.data["created_by"])
		assert.NotEmpty(event.data["changed_at"])
		if kind == "gmail" || kind == "imap" {
			assert.Positive(event.messageID)
			assert.Contains(event.data, "message_id")
		} else {
			assert.Zero(event.messageID)
			assert.NotContains(event.data, "message_id")
		}
	}
}

func TestMCPDraftProducerGmailOnlyConfirmedTransitions(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f := draftProducerFixture(t, "gmail")
	ctx := t.Context()
	receipt := store.GmailDraftReceipt{SourceID: f.Source.ID, GmailDraftID: "provider-draft", GmailMessageID: "draft-message-1", ThreadID: "draft-conversation"}
	draft, err := f.Store.PersistGmailDraftContext(ctx, receipt, nil, draftMessageBuild(f, receipt.GmailMessageID, "first"))
	require.NoError(err)
	assert.Len(readProducerEvents(t, f.Store, "msgvault.draft_changed"), 1)
	_, err = f.Store.ClaimGmailDraftContext(ctx, draft.DraftID, 1, store.GmailDraftOperationEdit, []byte("second"))
	require.NoError(err)
	require.NoError(f.Store.RecordGmailDraftOutcomeContext(ctx, draft.DraftID, 1, "accepted_local_failed", "draft-message-2"))
	assert.Len(readProducerEvents(t, f.Store, "msgvault.draft_changed"), 1)
	_, err = f.Store.PublishGmailDraftReplacementContext(ctx, draft.DraftID, 1, "draft-message-2", nil, draftMessageBuild(f, "draft-message-2", "second"))
	require.NoError(err)
	_, err = f.Store.ClaimGmailDraftContext(ctx, draft.DraftID, 2, store.GmailDraftOperationDelete, nil)
	require.NoError(err)
	require.NoError(f.Store.RecordGmailDraftOutcomeContext(ctx, draft.DraftID, 2, "delete_outcome_unknown", ""))
	assert.Len(readProducerEvents(t, f.Store, "msgvault.draft_changed"), 2)
	_, err = f.Store.FinishGmailDraftDeleteContext(ctx, draft.DraftID, 2)
	require.NoError(err)
	assertDraftTransitions(t, f, "gmail", draft.DraftID)
}

func TestMCPDraftProducerIMAPOnlyConfirmedTransitions(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f := draftProducerFixture(t, "imap")
	ctx := t.Context()
	receipt := store.IMAPDraftReceipt{SourceID: f.Source.ID, Mailbox: "Drafts", UIDValidity: 9, UID: 11}
	draft, err := f.Store.PersistIMAPDraftContext(ctx, receipt, nil, draftMessageBuild(f, store.IMAPDraftSourceMessageID(receipt), "first"))
	require.NoError(err)
	_, err = f.Store.ClaimIMAPDraftContext(ctx, draft.DraftID, 1, store.IMAPDraftOperationEdit, []byte("second"))
	require.NoError(err)
	replacement := receipt
	replacement.UID++
	require.NoError(f.Store.RecordIMAPDraftOutcomeContext(ctx, draft.DraftID, 1, store.IMAPDraftCodeCleanup, &replacement))
	assert.Len(readProducerEvents(t, f.Store, "msgvault.draft_changed"), 1)
	_, err = f.Store.PublishIMAPDraftReplacementContext(ctx, draft.DraftID, 1, nil, draftMessageBuild(f, store.IMAPDraftSourceMessageID(replacement), "second"))
	require.NoError(err)
	// Publication retains the old message's cleanup claim at revision 2.
	// Settle its confirmed absence before a new delete can be claimed.
	require.NoError(f.Store.RecordIMAPDraftOutcomeContext(ctx, draft.DraftID, 2, store.IMAPDraftCodeRemoved, nil))
	_, err = f.Store.FinishIMAPDraftRemovalContext(ctx, draft.DraftID, 2)
	require.NoError(err)
	assert.Len(readProducerEvents(t, f.Store, "msgvault.draft_changed"), 2, "settling an already-published edit adds no transition")
	_, err = f.Store.ClaimIMAPDraftContext(ctx, draft.DraftID, 2, store.IMAPDraftOperationDelete, nil)
	require.NoError(err)
	require.NoError(f.Store.RecordIMAPDraftOutcomeContext(ctx, draft.DraftID, 2, store.IMAPDraftCodeRemoved, nil))
	assert.Len(readProducerEvents(t, f.Store, "msgvault.draft_changed"), 2)
	_, err = f.Store.FinishIMAPDraftRemovalContext(ctx, draft.DraftID, 2)
	require.NoError(err)
	assertDraftTransitions(t, f, "imap", draft.DraftID)
}

func TestMCPDraftProducerBeeperFirstWriteAndAbort(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f := draftProducerFixture(t, "beeper")
	ctx := t.Context()
	draft, err := f.Store.CreateBeeperDraftContext(ctx, f.Source.ID, "draft-conversation", "first")
	require.NoError(err)
	assert.Empty(readProducerEvents(t, f.Store, "msgvault.draft_changed"))
	_, err = f.Store.AbortBeeperDraftContext(ctx, draft.DraftID, 1)
	require.NoError(err)
	assert.Empty(readProducerEvents(t, f.Store, "msgvault.draft_changed"))
	draft, err = f.Store.CreateBeeperDraftContext(ctx, f.Source.ID, "draft-conversation", "first")
	require.NoError(err)
	_, err = f.Store.FinishBeeperDraftContext(ctx, draft.DraftID, 1, "first")
	require.NoError(err)
	_, err = f.Store.ClaimBeeperDraftContext(ctx, draft.DraftID, 1, store.BeeperDraftOperationEdit, "second")
	require.NoError(err)
	assert.Len(readProducerEvents(t, f.Store, "msgvault.draft_changed"), 1)
	_, err = f.Store.FinishBeeperDraftContext(ctx, draft.DraftID, 1, "second")
	require.NoError(err)
	_, err = f.Store.ClaimBeeperDraftContext(ctx, draft.DraftID, 2, store.BeeperDraftOperationDelete, "")
	require.NoError(err)
	_, err = f.Store.FinishBeeperDraftContext(ctx, draft.DraftID, 2, "")
	require.NoError(err)
	assertDraftTransitions(t, f, "beeper", draft.DraftID)
}

func TestMCPDraftProducerBeeperMissingArchivedConversationMuted(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f := draftProducerFixture(t, "beeper")
	draft, err := f.Store.CreateBeeperDraftContext(t.Context(), f.Source.ID, "not-in-archive", "first")
	require.NoError(err)
	_, err = f.Store.FinishBeeperDraftContext(t.Context(), draft.DraftID, 1, "first")
	require.NoError(err)
	assert.Empty(readProducerEvents(t, f.Store, "msgvault.draft_changed"))
	var count int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM conversations WHERE source_id = ? AND source_conversation_id = ?`), f.Source.ID, "not-in-archive").Scan(&count))
	assert.Zero(count)
}

func TestMCPDraftProducerChatDeleteRetainsScopeAndLastRevision(t *testing.T) {
	require := Require.New(t)

	f := draftProducerFixture(t, "slack")
	draft, err := f.Store.CreateChatDraftContext(t.Context(), f.ConvID, 0, "first", func(string, string) error { return nil })
	require.NoError(err)
	_, err = f.Store.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "second")
	require.NoError(err)
	require.NoError(f.Store.DeleteChatDraftContext(t.Context(), draft.DraftID, 2))
	_, err = f.Store.GetChatDraftContext(t.Context(), draft.DraftID)
	require.ErrorIs(err, store.ErrChatDraftNotFound)
	assertDraftTransitions(t, f, "chat", draft.DraftID)
}

func TestMCPDraftProducerAbortedEmailClaimsMuted(t *testing.T) {
	t.Run("gmail", func(t *testing.T) {
		require := Require.New(t)
		assert := Assert.New(t)

		f := draftProducerFixture(t, "gmail")
		receipt := store.GmailDraftReceipt{SourceID: f.Source.ID, GmailDraftID: "abort-draft", GmailMessageID: "abort-message", ThreadID: "draft-conversation"}
		draft, err := f.Store.PersistGmailDraftContext(t.Context(), receipt, nil, draftMessageBuild(f, receipt.GmailMessageID, "first"))
		require.NoError(err)
		_, err = f.Store.ClaimGmailDraftContext(t.Context(), draft.DraftID, 1, store.GmailDraftOperationEdit, []byte("second"))
		require.NoError(err)
		_, err = f.Store.AbortGmailDraftContext(t.Context(), draft.DraftID, 1)
		require.NoError(err)
		assert.Len(readProducerEvents(t, f.Store, "msgvault.draft_changed"), 1)
	})
	t.Run("imap", func(t *testing.T) {
		require := Require.New(t)
		assert := Assert.New(t)

		f := draftProducerFixture(t, "imap")
		receipt := store.IMAPDraftReceipt{SourceID: f.Source.ID, Mailbox: "Drafts", UIDValidity: 9, UID: 11}
		draft, err := f.Store.PersistIMAPDraftContext(t.Context(), receipt, nil, draftMessageBuild(f, store.IMAPDraftSourceMessageID(receipt), "first"))
		require.NoError(err)
		_, err = f.Store.ClaimIMAPDraftContext(t.Context(), draft.DraftID, 1, store.IMAPDraftOperationEdit, []byte("second"))
		require.NoError(err)
		_, err = f.Store.AbortIMAPDraftContext(t.Context(), draft.DraftID, 1, "rejected")
		require.NoError(err)
		assert.Len(readProducerEvents(t, f.Store, "msgvault.draft_changed"), 1)
	})
}

func TestMCPDraftProducerJournalFailureRollsBackInitialEmailSnapshot(t *testing.T) {
	for _, kind := range []string{"gmail", "imap"} {
		t.Run(kind, func(t *testing.T) {
			require := Require.New(t)
			assert := Assert.New(t)
			f := draftProducerFixture(t, kind)
			var err error
			// The journal insert follows the message snapshot and draft ownership writes.
			if f.Store.IsPostgreSQL() {
				_, err = f.Store.DB().Exec(`CREATE FUNCTION reject_draft_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic draft event failure'; END $$`)
				require.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE TRIGGER reject_draft_event AFTER INSERT ON mcp_event_log FOR EACH ROW EXECUTE FUNCTION reject_draft_event()`)
			} else {
				_, err = f.Store.DB().Exec(`CREATE TRIGGER reject_draft_event AFTER INSERT ON mcp_event_log BEGIN SELECT RAISE(ABORT, 'synthetic draft event failure'); END`)
			}
			require.NoError(err)
			persist := func() error {
				if kind == "gmail" {
					receipt := store.GmailDraftReceipt{SourceID: f.Source.ID, GmailDraftID: "failed-draft", GmailMessageID: "failed-message", ThreadID: "draft-conversation"}
					_, err := f.Store.PersistGmailDraftContext(t.Context(), receipt, nil, draftMessageBuild(f, receipt.GmailMessageID, "Synthetic draft body"))
					return err
				}
				receipt := store.IMAPDraftReceipt{SourceID: f.Source.ID, Mailbox: "Drafts", UIDValidity: 9, UID: 11}
				_, err := f.Store.PersistIMAPDraftContext(t.Context(), receipt, nil, draftMessageBuild(f, store.IMAPDraftSourceMessageID(receipt), "Synthetic draft body"))
				return err
			}
			require.ErrorContains(persist(), "events_storage_unavailable")
			for _, table := range []string{"messages", kind + "_drafts", "mcp_event_log"} {
				var count int
				require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM `+table+` WHERE source_id=?`), f.Source.ID).Scan(&count))
				assert.Zero(count, "%s must roll back with the failed journal insert", table)
			}

			dropTrigger := `DROP TRIGGER reject_draft_event`
			if f.Store.IsPostgreSQL() {
				dropTrigger += ` ON mcp_event_log`
			}
			_, err = f.Store.DB().Exec(dropTrigger)
			require.NoError(err)
			require.NoError(persist(), "the same valid snapshot succeeds without the injected failure")
			for _, table := range []string{"messages", kind + "_drafts", "mcp_event_log"} {
				var count int
				require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM `+table+` WHERE source_id=?`), f.Source.ID).Scan(&count))
				assert.Equal(1, count, table)
			}
		})
	}
}

func TestMCPEmailDraftPhysicalDeletionCascades(t *testing.T) {
	for _, kind := range []string{"gmail", "imap"} {
		t.Run(kind, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := draftProducerFixture(t, kind)
			var messageID int64
			var draftID string
			if kind == "gmail" {
				receipt := store.GmailDraftReceipt{SourceID: f.Source.ID, GmailDraftID: "removed-draft", GmailMessageID: "removed-message", ThreadID: "draft-conversation"}
				draft, err := f.Store.PersistGmailDraftContext(t.Context(), receipt, nil, draftMessageBuild(f, receipt.GmailMessageID, "first"))
				require.NoError(err)
				messageID, draftID = draft.CurrentMessageID, draft.DraftID
			} else {
				receipt := store.IMAPDraftReceipt{SourceID: f.Source.ID, Mailbox: "Drafts", UIDValidity: 9, UID: 11}
				draft, err := f.Store.PersistIMAPDraftContext(t.Context(), receipt, nil, draftMessageBuild(f, store.IMAPDraftSourceMessageID(receipt), "first"))
				require.NoError(err)
				messageID, draftID = draft.CurrentMessageID, draft.DraftID
			}
			_, err := f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM messages WHERE id=?`), messageID)
			require.NoError(err)
			var count int
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM `+kind+`_drafts WHERE draft_id=?`), draftID).Scan(&count))
			assert.Zero(count, "the managed draft cannot survive physical deletion of its archived message")
		})
	}
}
