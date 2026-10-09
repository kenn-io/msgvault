package store_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func insertMergeDraft(t *testing.T, st *store.Store, sourceID int64, table string, discarded, pending bool) {
	t.Helper()
	discardedAt := any(nil)
	if discarded {
		discardedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	conversationID, err := st.EnsureConversationWithType(sourceID, "draft-thread", "email_thread", "Draft")
	require.NoError(t, err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: conversationID, SourceMessageID: "draft-message", MessageType: "email",
	})
	require.NoError(t, err)
	switch table {
	case "gmail_drafts":
		pendingOperation, pendingMessageID, pendingProviderID, pendingRaw := any(nil), any(nil), any(nil), any(nil)
		if pending {
			pendingOperation, pendingMessageID, pendingProviderID, pendingRaw = "edit", messageID, "old-gmail-message", []byte("synthetic pending draft bytes")
		}
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO gmail_drafts(
			draft_id, source_id, gmail_draft_id, current_message_id, current_gmail_message_id,
			thread_id, revision, discarded_at, pending_operation,
			pending_original_message_id, pending_original_gmail_message_id, pending_raw
		) VALUES (?, ?, 'gmail-draft', ?, 'current-gmail-message', 'thread', 1, ?, ?, ?, ?, ?)`),
			fmt.Sprintf("draft-%d", sourceID), sourceID, messageID, discardedAt,
			pendingOperation, pendingMessageID, pendingProviderID, pendingRaw)
	case "imap_drafts":
		pendingOperation, pendingMessageID, pendingMailbox, pendingUIDValidity, pendingUID, pendingRaw := any(nil), any(nil), any(nil), any(nil), any(nil), any(nil)
		if pending {
			pendingOperation, pendingMessageID, pendingMailbox, pendingUIDValidity, pendingUID, pendingRaw = "edit", messageID, "INBOX", 1, 1, []byte("synthetic pending draft bytes")
		}
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_drafts(
			draft_id, source_id, current_message_id, current_mailbox, current_uidvalidity,
			current_uid, revision, discarded_at, pending_operation,
			pending_original_message_id, pending_original_mailbox, pending_original_uidvalidity,
			pending_original_uid, pending_raw
		) VALUES (?, ?, ?, 'Drafts', 1, 1, 1, ?, ?, ?, ?, ?, ?, ?)`),
			fmt.Sprintf("draft-%d", sourceID), sourceID, messageID, discardedAt,
			pendingOperation, pendingMessageID, pendingMailbox, pendingUIDValidity, pendingUID, pendingRaw)
	case "beeper_drafts":
		pendingOperation, pendingRaw := any(nil), any(nil)
		if pending {
			pendingOperation, pendingRaw = "edit", []byte("synthetic pending draft bytes")
		}
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO beeper_drafts(
			draft_id, source_id, chat_id, text, revision, discarded_at, pending_operation, pending_raw
		) VALUES (?, ?, '!draft:example.test', 'draft', 1, ?, ?, ?)`),
			fmt.Sprintf("draft-%d", sourceID), sourceID, discardedAt, pendingOperation, pendingRaw)
	case "chat_drafts":
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO chat_drafts(
			draft_id, source_id, conversation_id, source_conversation_id,
			conversation_type, body, revision
		) VALUES (?, ?, ?, 'draft-thread', 'email_thread', 'synthetic draft', 1)`),
			fmt.Sprintf("draft-%d", sourceID), sourceID, conversationID)
	default:
		require.FailNow(t, "unknown draft table", table)
	}
	require.NoError(t, err)
}

func TestMergeSourcesBlocksActiveDraftsAndRetainsDiscardedReceipts(t *testing.T) {
	tests := []struct {
		name      string
		table     string
		discarded bool
		pending   bool
		wantBlock bool
	}{
		{name: "active Gmail receipt", table: "gmail_drafts", wantBlock: true},
		{name: "pending Gmail operation", table: "gmail_drafts", pending: true, wantBlock: true},
		{name: "discarded Gmail receipt", table: "gmail_drafts", discarded: true},
		{name: "active IMAP receipt", table: "imap_drafts", wantBlock: true},
		{name: "discarded IMAP receipt", table: "imap_drafts", discarded: true},
		{name: "active Beeper receipt", table: "beeper_drafts", wantBlock: true},
		{name: "discarded Beeper receipt", table: "beeper_drafts", discarded: true},
		{name: "active local chat draft", table: "chat_drafts", wantBlock: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			sourceType := "gmail"
			switch test.table {
			case "imap_drafts":
				sourceType = "imap"
			case "beeper_drafts", "chat_drafts":
				sourceType = "beeper"
			}
			from, err := st.GetOrCreateSource(sourceType, "history@example.test")
			require.NoError(err)
			into, err := st.GetOrCreateSource(sourceType, "live@example.test")
			require.NoError(err)
			insertMergeDraft(t, st, from.ID, test.table, test.discarded, test.pending)

			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
			if test.wantBlock {
				require.ErrorIs(err, store.ErrSourceMergeInvalid)
				return
			}
			require.NoError(err)
			var retainedSourceID int64
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT source_id FROM `+test.table+` WHERE source_id = ?`), from.ID).Scan(&retainedSourceID))
			assert.Equal(from.ID, retainedSourceID, "discarded provider receipts remain owned by the retired source")
		})
	}
}

func TestMergeSourcesSafety(t *testing.T) {
	for _, mode := range []string{"same", "missing", "cross-type", "execution", "abandoned", "pending", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("beeper", "history-example")
			require.NoError(err)
			intoType := "beeper"
			if mode == "cross-type" {
				intoType = "mbox"
			}
			into, err := st.GetOrCreateSource(intoType, "live-example")
			require.NoError(err)
			req := store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true}
			ctx := context.Background()
			switch mode {
			case "same":
				req.IntoSourceID = from.ID
			case "missing":
				req.FromSourceID = 99999
			case "execution":
				execution, err := st.AcquireSyncExecutionContext(ctx, from.ID)
				require.NoError(err)
				t.Cleanup(func() { require.NoError(execution.Release()) })
			case "abandoned":
				_, err := st.DB().Exec(st.Rebind(`INSERT INTO sync_runs(source_id, sync_type, started_at, status) VALUES (?, 'full', ?, 'running')`), from.ID, time.Now().UTC())
				require.NoError(err)
			case "pending":
				_, err := st.DB().Exec(st.Rebind(`INSERT INTO sync_operations(id, source_id, status) VALUES ('example-pending', ?, 'pending')`), into.ID)
				require.NoError(err)
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			for _, dry := range []bool{true, false} {
				req.DryRun = dry
				_, err := st.MergeSourcesContext(ctx, req)
				require.Error(err)
			}
			settings, err := st.GetSourceSettingsContext(context.Background(), from.ID)
			require.NoError(err)
			assert.Zero(settings.MergedIntoSourceID)
			if mode == "abandoned" {
				var status string
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT status FROM sync_runs WHERE source_id = ?`), from.ID).Scan(&status))
				assert.Equal("running", status, "preview/rejected execution must not recover abandoned history")
			}
		})
	}
}

func TestMergeSourcesEmptyDryRunAndIdempotency(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	require.NoError(st.SetArchiveMarker(ctx, store.BeeperReanchorMarkerKey(from.ID), "synthetic anchors"))
	req := store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true}
	preview, err := st.MergeSourcesContext(ctx, req)
	require.NoError(err)
	assert.True(preview.DryRun)
	before, err := st.GetSourceSettingsContext(ctx, from.ID)
	require.NoError(err)
	assert.Zero(before.MergedIntoSourceID)
	var journal int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM source_merges`).Scan(&journal))
	assert.Zero(journal)
	req.DryRun = false
	result, err := st.MergeSourcesContext(ctx, req)
	require.NoError(err)
	assert.Equal(from.ID, result.FromSourceID)
	after, err := st.GetSourceSettingsContext(ctx, from.ID)
	require.NoError(err)
	assert.Equal(into.ID, after.MergedIntoSourceID)
	assert.True(after.ReanchorRequired, "original marker is retained as evidence")
	again, err := st.MergeSourcesContext(ctx, req)
	require.NoError(err)
	assert.True(again.AlreadyMerged)
	assert.Equal(result.MessagesMoved, again.MessagesMoved)
	_, err = st.GetOrCreateSource("beeper", "history-example")
	require.ErrorIs(err, store.ErrSourceRetired)
	_, err = st.StartSync(from.ID, "full")
	require.ErrorIs(err, store.ErrSourceRetired)
	_, err = st.EnsureConversation(from.ID, "new-chat", "Example")
	require.Error(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO conversations(source_id, source_conversation_id, conversation_type) VALUES (?, 'direct', 'direct_chat')`), from.ID)
	require.Error(err, "database admission also blocks direct importer writes")
	third, err := st.GetOrCreateSource("beeper", "third-example")
	require.NoError(err)
	req.IntoSourceID = third.ID
	_, err = st.MergeSourcesContext(ctx, req)
	require.Error(err)
	req.FromSourceID, req.IntoSourceID = third.ID, from.ID
	_, err = st.MergeSourcesContext(ctx, req)
	require.Error(err)
}

func TestConcurrentMergeRetryReturnsSavedReportAfterJournalRace(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	req := store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID}

	retryAtJournal := make(chan struct{})
	releaseRetry := make(chan struct{})
	var hookCalls atomic.Int32
	releaseOnce := sync.Once{}
	openRetry := func() { releaseOnce.Do(func() { close(releaseRetry) }) }
	defer openRetry()
	restoreHook := st.SetSourceMergeAfterJournalCheckHookForTest(func() {
		if hookCalls.Add(1) == 1 {
			close(retryAtJournal)
			<-releaseRetry
		}
	})
	defer restoreHook()

	type mergeOutcome struct {
		result store.SourceMergeResult
		err    error
	}
	retryDone := make(chan mergeOutcome, 1)
	go func() {
		result, err := st.MergeSourcesContext(t.Context(), req)
		retryDone <- mergeOutcome{result: result, err: err}
	}()
	select {
	case <-retryAtJournal:
	case <-time.After(10 * time.Second):
		require.FailNow("retry did not reach the initial journal check")
	}

	first, err := st.MergeSourcesContext(t.Context(), req)
	openRetry()
	var retry mergeOutcome
	select {
	case retry = <-retryDone:
	case <-time.After(10 * time.Second):
		require.FailNow("concurrent retry did not finish")
	}
	require.NoError(err)
	require.NoError(retry.err)
	assert.False(first.AlreadyMerged)
	retryExpected := first
	retryExpected.AlreadyMerged = true
	assert.Equal(retryExpected, retry.result)
}

func TestMergeSourcesCanonicalizesRFC822MessageIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("gmail", "history@example.test")
	require.NoError(err)
	into, err := st.GetOrCreateSource("gmail", "live@example.test")
	require.NoError(err)

	survivor := emailMergeFixtureMessage(t, st, into.ID, "live-message", []string{"recipient@example.test"}, true)
	historical := emailMergeFixtureMessage(t, st, from.ID, "history-message", []string{"recipient@example.test"}, true)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), "<shared@example.test>", survivor)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), "shared@example.test", historical)
	require.NoError(err)

	result, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: from.ID, IntoSourceID: into.ID,
	})
	require.NoError(err)
	assert.Equal(int64(1), result.DuplicatesHidden)
	var hidden bool
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT deleted_at IS NOT NULL FROM messages WHERE id = ?`), historical).Scan(&hidden))
	assert.True(hidden)
	var basis string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT match_basis FROM source_merge_messages WHERE message_id = ?`), historical).Scan(&basis))
	assert.Equal("rfc822-or-payload", basis)
}

func TestMergeSourcesConflictingRFC822EvidenceCannotRetargetDeletion(t *testing.T) {
	for _, test := range []struct {
		name            string
		payloadConflict bool
	}{
		{name: "conflicting sender evidence"},
		{name: "conflicting payload", payloadConflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)

			from, err := st.GetOrCreateSource("gmail", "history@example.test")
			require.NoError(err)
			into, err := st.GetOrCreateSource("gmail", "live@example.test")
			require.NoError(err)
			survivor := emailMergeFixtureMessage(t, st, into.ID, "live-message", []string{"recipient@example.test"}, true)
			historical := emailMergeFixtureMessage(t, st, from.ID, "history-message", []string{"recipient@example.test"}, true)
			_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET rfc822_message_id = ? WHERE id IN (?, ?)`),
				"<collision@example.test>", survivor, historical)
			require.NoError(err)
			if test.payloadConflict {
				_, err = st.DB().Exec(st.Rebind(`UPDATE message_bodies SET body_text = CASE message_id
					WHEN ? THEN 'legitimate destination payload' ELSE 'forged historical payload' END
					WHERE message_id IN (?, ?)`), survivor, survivor, historical)
				require.NoError(err)
			}
			attackerID, err := st.EnsureParticipant("attacker@example.test", "Example Attacker", "example.test")
			require.NoError(err)
			_, err = st.DB().Exec(st.Rebind(`INSERT INTO message_recipients(
				message_id, participant_id, recipient_type, email_address
			) VALUES (?, ?, 'from', 'attacker@example.test')`), historical, attackerID)
			require.NoError(err)

			engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
			destinationID := into.ID
			before, err := engine.GetDeletionTargetsByFilter(t.Context(), query.MessageFilter{
				SourceID: &destinationID, Sender: "attacker@example.test",
			})
			require.NoError(err)
			assert.Empty(before, "the destination message does not belong to the forged sender before the merge")

			result, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
				FromSourceID: from.ID, IntoSourceID: into.ID,
			})
			require.NoError(err)
			assert.Zero(result.DuplicatesHidden, "conflicting Message-ID evidence must not select a survivor")
			assert.Equal(int64(1), result.AmbiguousMatches, "the conflicting Message-ID must be reported as ambiguous")

			after, err := engine.GetDeletionTargetsByFilter(t.Context(), query.MessageFilter{
				SourceID: &destinationID, Sender: "attacker@example.test",
			})
			require.NoError(err)
			assert.Empty(after, "historical sender evidence must not retarget the live destination message for deletion")
			var copiedAttackerFromRows int
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_recipients
				WHERE message_id = ? AND recipient_type = 'from' AND email_address = 'attacker@example.test'`), survivor).
				Scan(&copiedAttackerFromRows))
			assert.Zero(copiedAttackerFromRows, "conflicting sender evidence must remain on the archived message")
		})
	}
}
