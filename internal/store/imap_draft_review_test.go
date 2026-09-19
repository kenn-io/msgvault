package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestManagedIMAPDraftPendingReopen(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, _, draft, _ := newReviewManagedDraft(t, "pending-reopen", 11, "original")
	candidate := []byte("From: alice@example.com\r\nTo: bob@example.com\r\n\r\ncandidate\r\n")

	claimed, err := st.ClaimIMAPDraftContext(context.Background(), draft.DraftID, draft.Revision, store.IMAPDraftOperationEdit, candidate)
	requirements.NoError(err)
	requirements.NotNil(claimed.Pending)
	assertions.Equal(store.IMAPDraftOperationEdit, claimed.Pending.Operation)
	assertions.Equal(draft.CurrentMessageID, claimed.Pending.OriginalMessageID)
	assertions.Equal(draft.CurrentReceipt, claimed.Pending.OriginalReceipt)
	assertions.Equal(candidate, claimed.Pending.Raw)

	dbPath := store.DBPathForTest(st)
	requirements.NoError(st.Close())
	reopenedStore, err := store.Open(dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { _ = reopenedStore.Close() })

	reopened, err := reopenedStore.GetIMAPDraftContext(context.Background(), draft.DraftID)
	requirements.NoError(err)
	requirements.NotNil(reopened.Pending)
	assertions.Equal(candidate, reopened.Pending.Raw)
	assertions.Equal(draft.CurrentReceipt, reopened.Pending.OriginalReceipt)
	assertions.Equal(int64(1), reopened.Revision)
	_, err = reopenedStore.ClaimIMAPDraftContext(context.Background(), draft.DraftID, draft.Revision, store.IMAPDraftOperationDelete, nil)
	requirements.ErrorIs(err, store.ErrIMAPDraftPending)
}

func TestManagedIMAPDraftRevision(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, source, draft, conversationID := newReviewManagedDraft(t, "revision", 21, "original")

	_, err := st.ClaimIMAPDraftContext(context.Background(), draft.DraftID, 2, store.IMAPDraftOperationEdit, []byte("candidate"))
	requirements.ErrorIs(err, store.ErrIMAPDraftRevision)
	second, err := store.Open(store.DBPathForTest(st))
	requirements.NoError(err)
	t.Cleanup(func() { _ = second.Close() })

	start := make(chan struct{})
	type claimResult struct {
		claimed store.IMAPDraft
		err     error
	}
	results := make(chan claimResult, 2)
	claim := func(client *store.Store, candidate []byte) {
		<-start
		claimed, claimErr := client.ClaimIMAPDraftContext(
			context.Background(), draft.DraftID, 1, store.IMAPDraftOperationEdit, candidate,
		)
		results <- claimResult{claimed: claimed, err: claimErr}
	}
	go claim(st, []byte("candidate"))
	go claim(second, []byte("other"))
	close(start)
	firstResult := <-results
	secondResult := <-results
	var winner claimResult
	wins := 0
	for _, result := range []claimResult{firstResult, secondResult} {
		if result.err == nil {
			wins++
			winner = result
			continue
		}
		requirements.ErrorIs(result.err, store.ErrIMAPDraftPending)
	}
	requirements.Equal(1, wins)
	requirements.NotNil(winner.claimed.Pending)
	requirements.Equal(int64(1), winner.claimed.Revision)
	_, err = st.AbortIMAPDraftContext(context.Background(), draft.DraftID, 1, "rejected")
	requirements.NoError(err)
	active, err := st.GetIMAPDraftContext(context.Background(), draft.DraftID)
	requirements.NoError(err)
	assertions.Equal(int64(1), active.Revision)
	requirements.Nil(active.Pending)

	candidateRaw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\n\r\ncandidate\r\n")
	claimed, err := st.ClaimIMAPDraftContext(context.Background(), draft.DraftID, 1, store.IMAPDraftOperationEdit, candidateRaw)
	requirements.NoError(err)
	requirements.NotNil(claimed.Pending)
	replacement := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 22}
	requirements.NoError(st.RecordIMAPDraftOutcomeContext(context.Background(), draft.DraftID, 1, "append_uidplus", &replacement))
	published, err := st.PublishIMAPDraftReplacementContext(context.Background(), draft.DraftID, 1, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(replacement),
				MessageType: store.MessageTypeEmail, ConversationID: conversationID,
			},
			BodyText: sql.NullString{String: "candidate", Valid: true},
			RawMIME:  candidateRaw,
		}
	})
	requirements.NoError(err)
	assertions.Equal(int64(2), published.Revision)
	requirements.NotNil(published.Pending)
	requirements.NoError(st.RecordIMAPDraftOutcomeContext(context.Background(), draft.DraftID, 2, store.IMAPDraftCodeRemoved, nil))
	finished, err := st.FinishIMAPDraftRemovalContext(context.Background(), draft.DraftID, 2)
	requirements.NoError(err)
	assertions.Equal(int64(2), finished.Revision)
	requirements.Nil(finished.Pending)
	assertions.Equal(published.CurrentMessageID, finished.CurrentMessageID)
}

func TestManagedIMAPDraftAbort(t *testing.T) {
	for _, scenario := range []string{"rejected", "cancelled", "remote_unknown", "accepted_unidentified", "created", "delete", "acknowledged", "advanced", "stale"} {
		t.Run(scenario, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st, _, draft, _ := newReviewManagedDraft(t, "abort-"+scenario, 71, "original")
			operation := store.IMAPDraftOperationEdit
			raw := []byte("candidate")
			if scenario == "delete" {
				operation = store.IMAPDraftOperationDelete
				raw = nil
			}
			_, err := st.ClaimIMAPDraftContext(t.Context(), draft.DraftID, 1, operation, raw)
			requirements.NoError(err)
			if scenario == "acknowledged" {
				receipt := store.IMAPDraftReceipt{SourceID: draft.SourceID, Mailbox: "Drafts", UIDValidity: 1, UID: 72}
				requirements.NoError(st.RecordIMAPDraftOutcomeContext(t.Context(), draft.DraftID, 1, "append_uidplus", &receipt))
			}
			if scenario == "advanced" {
				_, err = st.DB().Exec(st.Rebind("UPDATE imap_drafts SET current_uid = 72 WHERE draft_id = ?"), draft.DraftID)
				requirements.NoError(err)
			}
			outcome := scenario
			revision := int64(1)
			if scenario == "delete" || scenario == "acknowledged" || scenario == "advanced" || scenario == "stale" {
				outcome = "rejected"
			}
			if scenario == "stale" {
				revision = 2
			}
			active, err := st.AbortIMAPDraftContext(t.Context(), draft.DraftID, revision, outcome)
			if scenario == "rejected" || scenario == "cancelled" {
				requirements.NoError(err)
				assertions.Nil(active.Pending)
				assertions.Equal(draft.CurrentMessageID, active.CurrentMessageID)
				assertions.Equal(draft.CurrentReceipt, active.CurrentReceipt)
			} else {
				requirements.Error(err)
			}
			stored, err := st.GetIMAPDraftContext(t.Context(), draft.DraftID)
			requirements.NoError(err)
			assertions.Equal(int64(1), stored.Revision)
			if scenario == "rejected" || scenario == "cancelled" {
				assertions.Nil(stored.Pending)
			} else {
				assertions.NotNil(stored.Pending)
			}
		})
	}
}

func TestManagedIMAPDraftConstraints(t *testing.T) {
	requirements := require.New(t)
	st, _, draft, _ := newReviewManagedDraft(t, "constraints", 31, "original")
	update := func(query string, args ...any) error {
		_, err := st.DB().Exec(st.Rebind(query), args...)
		return err
	}

	for _, candidate := range []struct {
		name  string
		extra string
		args  []any
	}{
		{name: "original without operation"},
		{name: "edit bytes without operation", extra: ", pending_raw = ?", args: []any{[]byte("candidate")}},
		{name: "receipt without operation", extra: ", pending_replacement_mailbox = 'Drafts', pending_replacement_uidvalidity = 1, pending_replacement_uid = 32, pending_code = 'append_uidplus'"},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			requirements := require.New(t)
			tx, err := st.DB().Begin()
			requirements.NoError(err)
			defer func() { _ = tx.Rollback() }()
			args := append([]any{draft.CurrentMessageID}, candidate.args...)
			args = append(args, draft.DraftID)
			_, err = tx.Exec(st.Rebind(
				"UPDATE imap_drafts SET pending_operation = NULL, pending_original_message_id = ?, pending_original_mailbox = 'Drafts', pending_original_uidvalidity = 1, pending_original_uid = 31"+candidate.extra+" WHERE draft_id = ?"), args...)
			requirements.Error(err)
		})
	}

	requirements.Error(update(`UPDATE imap_drafts SET revision = 0 WHERE draft_id = ?`, draft.DraftID))
	requirements.Error(update(`
		UPDATE imap_drafts
		SET pending_operation = 'edit', pending_original_message_id = ?
		WHERE draft_id = ?
	`, draft.CurrentMessageID, draft.DraftID))
	requirements.Error(update(`
		UPDATE imap_drafts
		SET pending_replacement_mailbox = 'Drafts'
		WHERE draft_id = ?
	`, draft.DraftID))
	requirements.Error(update(`UPDATE imap_drafts SET current_uid = 0 WHERE draft_id = ?`, draft.DraftID))
	fetched, err := st.GetIMAPDraftContext(context.Background(), draft.DraftID)
	requirements.NoError(err)
	requirements.Equal(int64(1), fetched.Revision)
	requirements.Nil(fetched.Pending)
}

func TestManagedIMAPDraftRetention(t *testing.T) {
	testutil.SkipIfPostgres(t, "archive GC is SQLite-only")
	requirements := require.New(t)
	assertions := assert.New(t)
	st, source, draft, conversationID := newReviewManagedDraft(t, "retention", 41, "managed")
	_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), draft.CurrentMessageID)
	requirements.NoError(err)
	controlID, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: source.ID, SourceMessageID: "unowned-control", ConversationID: conversationID,
			MessageType: store.MessageTypeEmail,
		},
		BodyText: sql.NullString{String: "control", Valid: true},
		RawMIME:  []byte("From: control@example.com\r\n\r\ncontrol\r\n"),
	})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), controlID)
	requirements.NoError(err)
	plan, err := st.PlanGCContext(context.Background())
	requirements.NoError(err)
	assertions.Equal(int64(1), plan.SourceDeleted)
	assertions.Equal([]int64{controlID}, plan.SourceDeletedIDs)
	_, err = st.ExecuteGCContext(context.Background(), plan)
	requirements.NoError(err)
	_, err = st.GetMessageContext(context.Background(), draft.CurrentMessageID)
	requirements.NoError(err)
	_, err = st.GetMessageContext(context.Background(), controlID)
	requirements.ErrorContains(err, "message not found")

	purge := newReviewManagedDraftOnStore(t, st, "dedup-purge", 42, "purge")
	purgeDraft := purge.draft
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages SET deleted_at = CURRENT_TIMESTAMP, delete_batch_id = ? WHERE id = ?
	`), "review-batch", purgeDraft.CurrentMessageID)
	requirements.NoError(err)
	deleted, err := st.DeleteDedupedBatch("review-batch")
	requirements.NoError(err)
	assertions.Equal(int64(1), deleted)
	_, err = st.GetIMAPDraftContext(context.Background(), purgeDraft.DraftID)
	requirements.ErrorIs(err, store.ErrIMAPDraftNotFound)
}

func TestManagedIMAPDraftSourceCascade(t *testing.T) {
	requirements := require.New(t)
	st, source, draft, _ := newReviewManagedDraft(t, "source-cascade", 51, "source")
	requirements.NoError(st.RemoveSource(source.ID))
	_, err := st.GetIMAPDraftContext(context.Background(), draft.DraftID)
	requirements.ErrorIs(err, store.ErrIMAPDraftNotFound)
}

func newReviewManagedDraft(
	t *testing.T,
	name string,
	uid uint32,
	body string,
) (*store.Store, *store.Source, store.IMAPDraft, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	fixture := newReviewManagedDraftOnStore(t, st, name, uid, body)
	return fixture.store, fixture.source, fixture.draft, fixture.conversationID
}

type reviewManagedDraftFixture struct {
	store          *store.Store
	source         *store.Source
	draft          store.IMAPDraft
	conversationID int64
}

func newReviewManagedDraftOnStore(
	t *testing.T,
	st *store.Store,
	name string,
	uid uint32,
	body string,
) reviewManagedDraftFixture {
	t.Helper()
	requirements := require.New(t)
	source, err := st.GetOrCreateSource("imap", fmt.Sprintf("imap://%s@example.com:143", name))
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, name, name)
	requirements.NoError(err)
	receipt := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: uid}
	raw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nContent-Type: text/plain\r\n\r\n" + body + "\r\n")
	draft, err := st.PersistIMAPDraftContext(context.Background(), receipt, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
				MessageType: store.MessageTypeEmail, ConversationID: conversationID,
			},
			BodyText: sql.NullString{String: body, Valid: true}, RawMIME: raw,
		}
	})
	requirements.NoError(err)
	return reviewManagedDraftFixture{store: st, source: source, draft: draft, conversationID: conversationID}
}
