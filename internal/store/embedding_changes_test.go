package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type embeddingJournalFixture struct {
	store             *store.Store
	sourceID          int64
	oldConversationID int64
	newConversationID int64
	aliceID           int64
	bobID             int64
}

func newEmbeddingJournalFixture(t *testing.T) embeddingJournalFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	require.NoError(t, st.EnableEmbeddingChangeJournal(t.Context()))
	source, err := st.GetOrCreateSource("beeper", "synthetic-account")
	require.NoError(t, err)
	oldConversationID, err := st.EnsureConversationWithType(
		source.ID, "chat-old", "group_chat", "Old chat")
	require.NoError(t, err)
	newConversationID, err := st.EnsureConversationWithType(
		source.ID, "chat-new", "group_chat", "New chat")
	require.NoError(t, err)
	aliceID, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(t, err)
	bobID, err := st.EnsureParticipant("bob@example.test", "Bob", "example.test")
	require.NoError(t, err)
	return embeddingJournalFixture{
		store:             st,
		sourceID:          source.ID,
		oldConversationID: oldConversationID,
		newConversationID: newConversationID,
		aliceID:           aliceID,
		bobID:             bobID,
	}
}

func TestEmbeddingChangeJournal_DisabledUntilContextualUse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "journal-opt-in")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "ordinary", "Ordinary")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, SourceMessageID: "journal-disabled",
		ConversationID: conversationID, MessageType: "email",
		Subject: sql.NullString{String: "Synthetic subject", Valid: true},
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(id,
		sql.NullString{String: "Synthetic body", Valid: true}, sql.NullString{}))
	assert.Zero(latestEmbeddingChangeSequence(t, st))
	assert.Empty(embeddingChangesAfter(t, st, 0))

	require.NoError(st.EnableEmbeddingChangeJournal(t.Context()))
	require.NoError(st.UpsertMessageBody(id,
		sql.NullString{String: "Updated synthetic body", Valid: true}, sql.NullString{}))
	assert.NotEmpty(embeddingChangesAfter(t, st, 0))
}

func (f embeddingJournalFixture) insertMessage(
	t *testing.T, sourceMessageID, messageType string, conversationID int64, sentAt time.Time,
) int64 {
	t.Helper()
	id, err := f.store.UpsertMessage(&store.Message{
		SourceID:        f.sourceID,
		SourceMessageID: sourceMessageID,
		ConversationID:  conversationID,
		MessageType:     messageType,
		SentAt:          sql.NullTime{Time: sentAt, Valid: true},
		SenderID:        sql.NullInt64{Int64: f.aliceID, Valid: true},
		Snippet:         sql.NullString{String: "original text", Valid: true},
	})
	require.NoError(t, err)
	return id
}

func (f embeddingJournalFixture) materializeBody(t *testing.T, messageID int64, body string) {
	t.Helper()
	require.NoError(t, f.store.UpsertMessageBody(messageID,
		sql.NullString{String: body, Valid: true}, sql.NullString{}))
}

func latestEmbeddingChangeSequence(t *testing.T, st *store.Store) int64 {
	t.Helper()
	sequence, err := st.LatestEmbeddingChangeSequence(t.Context())
	require.NoError(t, err)
	return sequence
}

func embeddingChangesAfter(
	t *testing.T, st *store.Store, after int64,
) []store.EmbeddingChange {
	t.Helper()
	changes, err := st.ScanEmbeddingChanges(t.Context(), after, 100)
	require.NoError(t, err)
	return changes
}

func requireJournalScope(
	t *testing.T,
	change store.EmbeddingChange,
	messageID int64,
	oldConversationID, newConversationID sql.NullInt64,
	oldSentAt, newSentAt sql.NullTime,
) {
	t.Helper()
	assert.Equal(t, sql.NullInt64{Int64: messageID, Valid: true}, change.MessageID)
	assert.Equal(t, oldConversationID, change.OldConversationID)
	assert.Equal(t, newConversationID, change.NewConversationID)
	if oldSentAt.Valid {
		require.True(t, change.OldSentAt.Valid)
		assert.True(t, change.OldSentAt.Time.Equal(oldSentAt.Time),
			"old sent_at: want %s, got %s", oldSentAt.Time, change.OldSentAt.Time)
	} else {
		assert.False(t, change.OldSentAt.Valid)
	}
	if newSentAt.Valid {
		require.True(t, change.NewSentAt.Valid)
		assert.True(t, change.NewSentAt.Time.Equal(newSentAt.Time),
			"new sent_at: want %s, got %s", newSentAt.Time, change.NewSentAt.Time)
	} else {
		assert.False(t, change.NewSentAt.Valid)
	}
}

func requireJournalTypes(t *testing.T, change store.EmbeddingChange, oldType, newType sql.NullString) {
	t.Helper()
	assert.Equal(t, oldType, change.OldMessageType)
	assert.Equal(t, newType, change.NewMessageType)
}

func TestEmbeddingChangeJournal_ContextualMessageInsert(t *testing.T) {
	for _, messageType := range []string{"beeper", "meeting_transcript"} {
		t.Run(messageType, func(t *testing.T) {
			assert := assert.New(t)
			f := newEmbeddingJournalFixture(t)
			before := latestEmbeddingChangeSequence(t, f.store)
			sentAt := time.Date(2026, 8, 8, 9, 30, 0, 0,
				time.FixedZone("test-offset", 2*60*60))
			messageID := f.insertMessage(
				t, "insert-"+messageType, messageType, f.oldConversationID, sentAt)
			insertChanges := embeddingChangesAfter(t, f.store, before)
			require.Len(t, insertChanges, 1)
			assert.Equal(store.EmbeddingChangeMessageInsert, insertChanges[0].Kind)
			requireJournalTypes(t, insertChanges[0], sql.NullString{},
				sql.NullString{String: messageType, Valid: true})
			afterInsert := latestEmbeddingChangeSequence(t, f.store)
			assert.Equal(before+1, afterInsert)
			f.materializeBody(t, messageID, "first embeddable body")

			changes := embeddingChangesAfter(t, f.store, before)
			require.Len(t, changes, 1)
			assert.Equal(afterInsert+1, changes[0].Sequence,
				"the body event must supersede the unconsumed bare-message event")
			assert.Equal(store.EmbeddingChangeMessageInsert, changes[0].Kind)
			requireJournalTypes(t, changes[0], sql.NullString{},
				sql.NullString{String: messageType, Valid: true})
			requireJournalScope(t, changes[0], messageID,
				sql.NullInt64{}, sql.NullInt64{Int64: f.oldConversationID, Valid: true},
				sql.NullTime{}, sql.NullTime{Time: sentAt, Valid: true})
			assert.Equal(time.UTC, changes[0].NewSentAt.Time.Location())
		})
	}
}

func TestEmbeddingChangeJournal_ContextualSoftDeleteAndRestoreClearInactiveScope(t *testing.T) {
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	sentAt := time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC)
	messageID := f.insertMessage(t, "contextual-soft-lifecycle", "beeper", f.oldConversationID, sentAt)
	f.materializeBody(t, messageID, "synthetic soft lifecycle body")

	before := latestEmbeddingChangeSequence(t, f.store)
	_, err := f.store.DB().Exec(f.store.Rebind(
		`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	require.NoError(err)
	changes := embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 1)
	requireJournalTypes(t, changes[0], sql.NullString{String: "beeper", Valid: true}, sql.NullString{})
	requireJournalScope(t, changes[0], messageID,
		sql.NullInt64{Int64: f.oldConversationID, Valid: true}, sql.NullInt64{},
		sql.NullTime{Time: sentAt, Valid: true}, sql.NullTime{})

	before = latestEmbeddingChangeSequence(t, f.store)
	_, err = f.store.DB().Exec(f.store.Rebind(
		`UPDATE messages SET deleted_at = NULL WHERE id = ?`), messageID)
	require.NoError(err)
	changes = embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 1)
	requireJournalTypes(t, changes[0], sql.NullString{}, sql.NullString{String: "beeper", Valid: true})
	requireJournalScope(t, changes[0], messageID,
		sql.NullInt64{}, sql.NullInt64{Int64: f.oldConversationID, Valid: true},
		sql.NullTime{}, sql.NullTime{Time: sentAt, Valid: true})
}

func TestEmbeddingChangeJournal_CoalescesSameScopeMessagePersistence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	before := latestEmbeddingChangeSequence(t, f.store)
	id, err := f.store.UpsertMessage(&store.Message{
		SourceID: f.sourceID, SourceMessageID: "ordinary-subject-body",
		ConversationID: f.oldConversationID, MessageType: "email",
		SentAt:  sql.NullTime{Time: time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC), Valid: true},
		Subject: sql.NullString{String: "Synthetic subject", Valid: true},
	})
	require.NoError(err)
	require.NoError(f.store.UpsertMessageBody(id,
		sql.NullString{String: "Synthetic body", Valid: true}, sql.NullString{}))

	changes := embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 1)
	assert.Equal(store.EmbeddingChangeMessageInsert, changes[0].Kind)
	assert.Equal(sql.NullInt64{Int64: id, Valid: true}, changes[0].MessageID)
}

func TestEmbeddingChangeJournal_PrunesOnlyConsumedPrefix(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	first := f.insertMessage(t, "prune-first", "email", f.oldConversationID,
		time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
	f.materializeBody(t, first, "first body")
	firstSequence := latestEmbeddingChangeSequence(t, f.store)
	second := f.insertMessage(t, "prune-second", "email", f.oldConversationID,
		time.Date(2026, 8, 8, 10, 30, 0, 0, time.UTC))
	f.materializeBody(t, second, "second body")
	latest := latestEmbeddingChangeSequence(t, f.store)

	pruned, err := f.store.PruneEmbeddingChangesThrough(t.Context(), firstSequence)

	require.NoError(err)
	assert.Positive(pruned)
	changes := embeddingChangesAfter(t, f.store, 0)
	require.Len(changes, 1)
	assert.Equal(sql.NullInt64{Int64: second, Valid: true}, changes[0].MessageID)
	assert.Equal(latest, latestEmbeddingChangeSequence(t, f.store),
		"retention must not rewind the commit-order clock")
}

func TestEmbeddingChangeJournal_BodylessOrdinaryLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	before := latestEmbeddingChangeSequence(t, f.store)
	messageID, err := f.store.UpsertMessage(&store.Message{
		SourceID: f.sourceID, SourceMessageID: "bodyless-ordinary", MessageType: "email",
		ConversationID: f.oldConversationID, Subject: sql.NullString{String: "Subject only", Valid: true},
		SentAt:   sql.NullTime{Time: time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC), Valid: true},
		SenderID: sql.NullInt64{Int64: f.aliceID, Valid: true},
	})
	require.NoError(err)

	changes := embeddingChangesAfter(t, f.store, before)
	require.NotEmpty(changes)
	assert.Equal(store.EmbeddingChangeMessageInsert, changes[0].Kind)
	before = latestEmbeddingChangeSequence(t, f.store)
	_, err = f.store.UpsertMessage(&store.Message{
		SourceID: f.sourceID, SourceMessageID: "bodyless-ordinary", MessageType: "email",
		ConversationID: f.oldConversationID, Subject: sql.NullString{String: "Edited subject", Valid: true},
		SentAt:   sql.NullTime{Time: time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC), Valid: true},
		SenderID: sql.NullInt64{Int64: f.aliceID, Valid: true},
	})
	require.NoError(err)
	changes = embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 1)
	assert.Equal(store.EmbeddingChangeMessageUpdate, changes[0].Kind)

	before = latestEmbeddingChangeSequence(t, f.store)
	_, err = f.store.UpsertMessage(&store.Message{
		SourceID: f.sourceID, SourceMessageID: "bodyless-ordinary", MessageType: "email",
		ConversationID: f.oldConversationID, Subject: sql.NullString{},
		SentAt:   sql.NullTime{Time: time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC), Valid: true},
		SenderID: sql.NullInt64{Int64: f.aliceID, Valid: true},
	})
	require.NoError(err)
	changes = embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 1)
	assert.Equal(store.EmbeddingChangeMessageUpdate, changes[0].Kind,
		"clearing the last embeddable field must retire the stale document")

	before = latestEmbeddingChangeSequence(t, f.store)
	_, err = f.store.DB().Exec(f.store.Rebind(
		`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	require.NoError(err)
	changes = embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 1)
	assert.Equal(store.EmbeddingChangeMessageUpdate, changes[0].Kind)
}

func TestEmbeddingChangeJournal_NewBodylessContextualMessage(t *testing.T) {
	for _, messageType := range []string{"beeper", "meeting_transcript"} {
		t.Run(messageType, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newEmbeddingJournalFixture(t)
			before := latestEmbeddingChangeSequence(t, f.store)

			messageID, err := f.store.UpsertMessage(&store.Message{
				SourceID:        f.sourceID,
				SourceMessageID: "bodyless-contextual-" + messageType,
				MessageType:     messageType,
				ConversationID:  f.oldConversationID,
				SentAt: sql.NullTime{
					Time:  time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC),
					Valid: true,
				},
			})
			require.NoError(err)

			changes := embeddingChangesAfter(t, f.store, before)
			require.Len(changes, 1)
			assert.Equal(store.EmbeddingChangeMessageInsert, changes[0].Kind)
			assert.Equal(sql.NullInt64{Int64: messageID, Valid: true}, changes[0].MessageID)
			requireJournalTypes(t, changes[0], sql.NullString{},
				sql.NullString{String: messageType, Valid: true})
		})
	}
}

func TestEmbeddingChangeJournal_EmbedGenResetJournalsBodylessOrdinary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	messageID, err := f.store.UpsertMessage(&store.Message{
		SourceID: f.sourceID, SourceMessageID: "bodyless-reset", MessageType: "email",
		ConversationID: f.oldConversationID,
		Subject:        sql.NullString{String: "Repaired subject", Valid: true},
	})
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(
		`UPDATE messages SET embed_gen = ? WHERE id = ?`), int64(42), messageID)
	require.NoError(err)
	before := latestEmbeddingChangeSequence(t, f.store)

	require.NoError(f.store.ResetEmbedGen(t.Context(), []int64{messageID}))

	changes := embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 1)
	assert.Equal(store.EmbeddingChangeMessageUpdate, changes[0].Kind)
	assert.Equal(sql.NullInt64{Int64: messageID, Valid: true}, changes[0].MessageID)
}

func TestEmbeddingChangeJournal_BodylessOrdinaryUpsertRollsBackWithJournalFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	postgres := store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB"))

	var before int64
	require.NoError(f.store.DB().QueryRow(
		`SELECT sequence FROM embedding_change_clock WHERE singleton = 1`).Scan(&before))
	insertSQL := `INSERT INTO embedding_changes (sequence, kind) VALUES (?, ?)`
	messageCountSQL := `SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`
	if postgres {
		insertSQL = `INSERT INTO embedding_changes (sequence, kind) VALUES ($1, $2)`
		messageCountSQL = `SELECT COUNT(*) FROM messages WHERE source_id = $1 AND source_message_id = $2`
	}
	_, err := f.store.DB().Exec(insertSQL,
		before+1, string(store.EmbeddingChangeMessageUpdate))
	require.NoError(err)

	_, err = f.store.UpsertMessage(&store.Message{
		SourceID: f.sourceID, SourceMessageID: "bodyless-atomic", MessageType: "email",
		ConversationID: f.oldConversationID,
		Subject:        sql.NullString{String: "Synthetic subject", Valid: true},
	})
	require.Error(err)
	assert.Contains(err.Error(), "bodyless message journal")

	var messages int
	require.NoError(f.store.DB().QueryRow(messageCountSQL,
		f.sourceID, "bodyless-atomic").Scan(&messages))
	assert.Zero(messages, "the message and its journal event must commit or roll back together")
	var after int64
	require.NoError(f.store.DB().QueryRow(
		`SELECT sequence FROM embedding_change_clock WHERE singleton = 1`).Scan(&after))
	assert.Equal(before, after, "a failed journal append must not advance the durable clock")
}

func TestEmbeddingChangeJournal_MessageLookupIndexRestoredByInitSchema(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	const indexName = "idx_embedding_changes_message_id"

	_, err := f.store.DB().Exec(`DROP INDEX IF EXISTS ` + indexName)
	require.NoError(err)
	require.NoError(f.store.InitSchema())

	var count int
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		require.NoError(f.store.DB().QueryRow(`
			SELECT COUNT(*) FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = $1`, indexName).Scan(&count))
	} else {
		require.NoError(f.store.DB().QueryRow(`
			SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, indexName).Scan(&count))
	}
	assert.Equal(1, count)
}

func TestEmbeddingChangeJournal_UnchangedMembershipSnapshotIsNoOp(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	messageID := f.insertMessage(t, "membership-snapshot", "beeper", f.oldConversationID,
		time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
	f.materializeBody(t, messageID, "membership body")
	require.NoError(f.store.EnsureConversationParticipant(f.oldConversationID, f.aliceID, "member"))
	before := latestEmbeddingChangeSequence(t, f.store)

	require.NoError(f.store.ReplaceConversationParticipants(f.oldConversationID,
		[]store.ConversationParticipantRef{{ParticipantID: f.aliceID, Role: "member"}}))
	assert.Equal(before, latestEmbeddingChangeSequence(t, f.store))
	assert.Empty(embeddingChangesAfter(t, f.store, before))
}

func TestEmbeddingChangeJournal_MembershipSnapshotEmitsOneConversationEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	messageID := f.insertMessage(t, "membership-change", "beeper", f.oldConversationID,
		time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
	f.materializeBody(t, messageID, "membership body")
	require.NoError(f.store.EnsureConversationParticipant(f.oldConversationID, f.aliceID, "member"))
	before := latestEmbeddingChangeSequence(t, f.store)

	require.NoError(f.store.ReplaceConversationParticipants(f.oldConversationID,
		[]store.ConversationParticipantRef{{ParticipantID: f.bobID, Role: "member"}}))
	changes := embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 1)
	assert.Equal(store.EmbeddingChangeConversationParticipant, changes[0].Kind)
	assert.Equal(sql.NullInt64{Int64: f.oldConversationID, Valid: true}, changes[0].OldConversationID)
	assert.Equal(sql.NullInt64{Int64: f.oldConversationID, Valid: true}, changes[0].NewConversationID)
	assert.False(changes[0].ParticipantID.Valid)
}

func TestEmbeddingChangeJournal_MembershipSnapshotAcquiresSQLiteWriterBeforeRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite snapshot-upgrade regression")
	}
	dbPath := filepath.Join(t.TempDir(), "membership-writer.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	require.NoError(st.EnableEmbeddingChangeJournal(t.Context()))
	source, err := st.GetOrCreateSource("beeper", "synthetic-membership-writer")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "membership-writer", "group_chat", "Synthetic chat")
	require.NoError(err)
	firstParticipant, err := st.EnsureParticipant(
		"first@example.test", "First", "example.test")
	require.NoError(err)
	secondParticipant, err := st.EnsureParticipant(
		"second@example.test", "Second", "example.test")
	require.NoError(err)
	require.NoError(st.EnsureConversationParticipant(conversationID, firstParticipant, "member"))

	holder, err := store.OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = holder.Close() })
	conn, err := holder.DB().Conn(t.Context())
	require.NoError(err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	require.NoError(err)
	committed := false
	t.Cleanup(func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	})
	_, err = conn.ExecContext(t.Context(),
		`UPDATE embedding_change_clock SET sequence = sequence + 1 WHERE singleton = 1`)
	require.NoError(err)

	result := make(chan error, 1)
	go func() {
		result <- st.ReplaceConversationParticipants(conversationID,
			[]store.ConversationParticipantRef{{ParticipantID: secondParticipant, Role: "member"}})
	}()

	var replaceErr error
	select {
	case replaceErr = <-result:
	case <-time.After(100 * time.Millisecond):
	}
	_, err = conn.ExecContext(t.Context(), "COMMIT")
	require.NoError(err)
	committed = true
	if replaceErr == nil {
		select {
		case replaceErr = <-result:
		case <-time.After(5 * time.Second):
			require.FailNow("membership replacement did not finish after releasing writer")
		}
	}
	require.NoError(replaceErr)

	var count int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM conversation_participants
		WHERE conversation_id = ? AND participant_id = ?`,
		conversationID, secondParticipant).Scan(&count))
	assert.Equal(1, count)
}

func TestEmbeddingChangeJournal_MembershipSnapshotBoundsSQLParameters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite variable-limit regression")
	}
	f := newEmbeddingJournalFixture(t)
	require.NoError(f.store.EnsureConversationParticipant(
		f.oldConversationID, f.aliceID, "member"))
	desired := make([]store.ConversationParticipantRef, 40)
	for i := range desired {
		participantID, err := f.store.EnsureParticipant(
			fmt.Sprintf("bounded-%d@example.test", i),
			fmt.Sprintf("Bounded %d", i), "example.test")
		require.NoError(err)
		desired[i] = store.ConversationParticipantRef{
			ParticipantID: participantID,
			Role:          "member",
		}
	}

	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	require.NoError(err)
	err = conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		require.True(ok, "driver connection is SQLite")
		sqliteConn.SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 32)
		return nil
	})
	require.NoError(err)
	require.NoError(conn.Close())

	require.NoError(f.store.ReplaceConversationParticipants(f.oldConversationID, desired))
	var count int
	require.NoError(f.store.DB().QueryRow(`
		SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ?`,
		f.oldConversationID).Scan(&count))
	assert.Equal(len(desired), count)
}

func TestEmbeddingChangeJournal_MessageTypeTransitionsPreserveOldAndNewKind(t *testing.T) {
	transitions := []struct {
		name    string
		oldType string
		newType string
	}{
		{name: "beeper to meeting", oldType: "beeper", newType: "meeting_transcript"},
		{name: "beeper to ordinary", oldType: "beeper", newType: "email"},
		{name: "meeting to beeper", oldType: "meeting_transcript", newType: "beeper"},
	}
	for _, transition := range transitions {
		t.Run(transition.name, func(t *testing.T) {
			f := newEmbeddingJournalFixture(t)
			id := f.insertMessage(t, "type-transition", transition.oldType, f.oldConversationID,
				time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
			f.materializeBody(t, id, "transition body")
			before := latestEmbeddingChangeSequence(t, f.store)
			_, err := f.store.DB().Exec(f.store.Rebind(
				`UPDATE messages SET message_type = ?, conversation_id = ? WHERE id = ?`),
				transition.newType, f.newConversationID, id)
			require.NoError(t, err)

			changes := embeddingChangesAfter(t, f.store, before)
			require.Len(t, changes, 1)
			assert.Equal(t, store.EmbeddingChangeMessageUpdate, changes[0].Kind)
			requireJournalTypes(t, changes[0],
				sql.NullString{String: transition.oldType, Valid: true},
				sql.NullString{String: transition.newType, Valid: true})
		})
	}
}

func TestEmbeddingChangeJournal_BodylessOrdinaryTransitionToContextualPreservesBothScopes(t *testing.T) {
	for _, newType := range []string{"beeper", "meeting_transcript"} {
		t.Run(newType, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newEmbeddingJournalFixture(t)
			sentAt := time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC)
			messageID, err := f.store.UpsertMessage(&store.Message{
				SourceID: f.sourceID, SourceMessageID: "bodyless-to-" + newType,
				ConversationID: f.oldConversationID, MessageType: "email",
				Subject:  sql.NullString{String: "Synthetic subject", Valid: true},
				SentAt:   sql.NullTime{Time: sentAt, Valid: true},
				SenderID: sql.NullInt64{Int64: f.aliceID, Valid: true},
			})
			require.NoError(err)
			_, err = f.store.DB().Exec(f.store.Rebind(
				`UPDATE messages SET embed_gen = ? WHERE id = ?`), int64(42), messageID)
			require.NoError(err)
			before := latestEmbeddingChangeSequence(t, f.store)

			updatedID, err := f.store.UpsertMessage(&store.Message{
				SourceID: f.sourceID, SourceMessageID: "bodyless-to-" + newType,
				ConversationID: f.oldConversationID, MessageType: newType,
				Subject:  sql.NullString{String: "Synthetic subject", Valid: true},
				SentAt:   sql.NullTime{Time: sentAt, Valid: true},
				SenderID: sql.NullInt64{Int64: f.aliceID, Valid: true},
			})
			require.NoError(err)
			assert.Equal(messageID, updatedID)
			var storedType string
			var embedGen sql.NullInt64
			require.NoError(f.store.DB().QueryRow(f.store.Rebind(
				`SELECT message_type, embed_gen FROM messages WHERE id = ?`), messageID).
				Scan(&storedType, &embedGen))
			assert.Equal(newType, storedType)
			assert.False(embedGen.Valid)

			changes := embeddingChangesAfter(t, f.store, before)
			require.Len(changes, 1)
			assert.Equal(store.EmbeddingChangeMessageUpdate, changes[0].Kind)
			requireJournalTypes(t, changes[0],
				sql.NullString{String: "email", Valid: true},
				sql.NullString{String: newType, Valid: true})
			requireJournalScope(t, changes[0], messageID,
				sql.NullInt64{Int64: f.oldConversationID, Valid: true},
				sql.NullInt64{Int64: f.oldConversationID, Valid: true},
				sql.NullTime{Time: sentAt, Valid: true},
				sql.NullTime{Time: sentAt, Valid: true})
		})
	}
}

func TestEmbeddingChangeJournal_MessageMutationsKeepOldAndNewScope(t *testing.T) {
	sentAt := time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC)
	movedAt := sentAt.Add(25 * time.Hour)
	tests := []struct {
		name        string
		apply       func(t *testing.T, f embeddingJournalFixture, messageID int64)
		want        func(f embeddingJournalFixture, messageID int64) (sql.NullInt64, sql.NullInt64, sql.NullTime, sql.NullTime)
		kind        store.EmbeddingChangeKind
		newInactive bool
	}{
		{
			name: "sender",
			apply: func(t *testing.T, f embeddingJournalFixture, messageID int64) {
				t.Helper()
				_, err := f.store.DB().Exec(f.store.Rebind(
					`UPDATE messages SET sender_id = ? WHERE id = ?`), f.bobID, messageID)
				require.NoError(t, err)
			},
			kind: store.EmbeddingChangeMessageUpdate,
		},
		{
			name: "time and UTC day",
			apply: func(t *testing.T, f embeddingJournalFixture, messageID int64) {
				t.Helper()
				_, err := f.store.DB().Exec(f.store.Rebind(
					`UPDATE messages SET sent_at = ? WHERE id = ?`), movedAt, messageID)
				require.NoError(t, err)
			},
			want: func(f embeddingJournalFixture, _ int64) (sql.NullInt64, sql.NullInt64, sql.NullTime, sql.NullTime) {
				return sql.NullInt64{Int64: f.oldConversationID, Valid: true},
					sql.NullInt64{Int64: f.oldConversationID, Valid: true},
					sql.NullTime{Time: sentAt, Valid: true},
					sql.NullTime{Time: movedAt, Valid: true}
			},
			kind: store.EmbeddingChangeMessageUpdate,
		},
		{
			name: "conversation move",
			apply: func(t *testing.T, f embeddingJournalFixture, messageID int64) {
				t.Helper()
				_, err := f.store.DB().Exec(f.store.Rebind(
					`UPDATE messages SET conversation_id = ? WHERE id = ?`),
					f.newConversationID, messageID)
				require.NoError(t, err)
			},
			want: func(f embeddingJournalFixture, _ int64) (sql.NullInt64, sql.NullInt64, sql.NullTime, sql.NullTime) {
				return sql.NullInt64{Int64: f.oldConversationID, Valid: true},
					sql.NullInt64{Int64: f.newConversationID, Valid: true},
					sql.NullTime{Time: sentAt, Valid: true},
					sql.NullTime{Time: sentAt, Valid: true}
			},
			kind: store.EmbeddingChangeMessageUpdate,
		},
		{
			name: "delete",
			apply: func(t *testing.T, f embeddingJournalFixture, messageID int64) {
				t.Helper()
				_, err := f.store.DB().Exec(f.store.Rebind(
					`DELETE FROM messages WHERE id = ?`), messageID)
				require.NoError(t, err)
			},
			want: func(f embeddingJournalFixture, _ int64) (sql.NullInt64, sql.NullInt64, sql.NullTime, sql.NullTime) {
				return sql.NullInt64{Int64: f.oldConversationID, Valid: true}, sql.NullInt64{},
					sql.NullTime{Time: sentAt, Valid: true}, sql.NullTime{}
			},
			kind:        store.EmbeddingChangeMessageDelete,
			newInactive: true,
		},
		{
			name: "source tombstone",
			apply: func(t *testing.T, f embeddingJournalFixture, messageID int64) {
				t.Helper()
				_, err := f.store.DB().Exec(f.store.Rebind(
					`UPDATE messages SET deleted_from_source_at = ? WHERE id = ?`), time.Now().UTC(), messageID)
				require.NoError(t, err)
			},
			want: func(f embeddingJournalFixture, _ int64) (sql.NullInt64, sql.NullInt64, sql.NullTime, sql.NullTime) {
				return sql.NullInt64{Int64: f.oldConversationID, Valid: true}, sql.NullInt64{},
					sql.NullTime{Time: sentAt, Valid: true}, sql.NullTime{}
			},
			kind:        store.EmbeddingChangeMessageUpdate,
			newInactive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEmbeddingJournalFixture(t)
			messageID := f.insertMessage(t, "message-mutation", "beeper", f.oldConversationID, sentAt)
			f.materializeBody(t, messageID, "original body")
			before := latestEmbeddingChangeSequence(t, f.store)
			tt.apply(t, f, messageID)

			changes := embeddingChangesAfter(t, f.store, before)
			require.Len(t, changes, 1)
			assert.Equal(t, tt.kind, changes[0].Kind)
			oldConversationID := sql.NullInt64{Int64: f.oldConversationID, Valid: true}
			newConversationID := oldConversationID
			oldTime := sql.NullTime{Time: sentAt, Valid: true}
			newTime := oldTime
			if tt.want != nil {
				oldConversationID, newConversationID, oldTime, newTime = tt.want(f, messageID)
			}
			requireJournalScope(t, changes[0], messageID,
				oldConversationID, newConversationID, oldTime, newTime)
			newType := sql.NullString{String: "beeper", Valid: true}
			if tt.newInactive {
				newType = sql.NullString{}
			}
			requireJournalTypes(t, changes[0],
				sql.NullString{String: "beeper", Valid: true}, newType)
		})
	}
}

func TestEmbeddingChangeJournal_FallbackTimestampMutationsKeepCanonicalScope(t *testing.T) {
	oldTime := time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC)
	newTime := oldTime.Add(25 * time.Hour)
	tests := []struct {
		name  string
		seed  string
		apply string
	}{
		{
			name:  "received_at",
			seed:  `UPDATE messages SET sent_at = NULL, received_at = ?, internal_date = ? WHERE id = ?`,
			apply: `UPDATE messages SET received_at = ? WHERE id = ?`,
		},
		{
			name:  "internal_date",
			seed:  `UPDATE messages SET sent_at = NULL, received_at = NULL, internal_date = ? WHERE id = ?`,
			apply: `UPDATE messages SET internal_date = ? WHERE id = ?`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newEmbeddingJournalFixture(t)
			messageID := f.insertMessage(t, "fallback-"+tt.name, "beeper", f.oldConversationID, oldTime)
			if tt.name == "received_at" {
				_, err := f.store.DB().Exec(f.store.Rebind(tt.seed), oldTime, oldTime.Add(-time.Hour), messageID)
				require.NoError(err)
			} else {
				_, err := f.store.DB().Exec(f.store.Rebind(tt.seed), oldTime, messageID)
				require.NoError(err)
			}
			f.materializeBody(t, messageID, "fallback timestamp body")
			before := latestEmbeddingChangeSequence(t, f.store)
			_, err := f.store.DB().Exec(f.store.Rebind(tt.apply), newTime, messageID)
			require.NoError(err)

			changes := embeddingChangesAfter(t, f.store, before)
			require.Len(changes, 1)
			assert.Equal(store.EmbeddingChangeMessageUpdate, changes[0].Kind)
			requireJournalScope(t, changes[0], messageID,
				sql.NullInt64{Int64: f.oldConversationID, Valid: true},
				sql.NullInt64{Int64: f.oldConversationID, Valid: true},
				sql.NullTime{Time: oldTime, Valid: true},
				sql.NullTime{Time: newTime, Valid: true})
		})
	}
}

func TestEmbeddingChangeJournal_MessageBodyMutations(t *testing.T) {
	tests := []struct {
		name  string
		seed  bool
		apply func(t *testing.T, f embeddingJournalFixture, messageID int64)
	}{
		{
			name: "insert",
			apply: func(t *testing.T, f embeddingJournalFixture, messageID int64) {
				t.Helper()
				require.NoError(t, f.store.UpsertMessageBody(messageID,
					sql.NullString{String: "inserted body", Valid: true}, sql.NullString{}))
			},
		},
		{
			name: "update",
			seed: true,
			apply: func(t *testing.T, f embeddingJournalFixture, messageID int64) {
				t.Helper()
				require.NoError(t, f.store.UpsertMessageBody(messageID,
					sql.NullString{String: "updated body", Valid: true}, sql.NullString{}))
			},
		},
		{
			name: "delete",
			seed: true,
			apply: func(t *testing.T, f embeddingJournalFixture, messageID int64) {
				t.Helper()
				_, err := f.store.DB().Exec(f.store.Rebind(
					`DELETE FROM message_bodies WHERE message_id = ?`), messageID)
				require.NoError(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEmbeddingJournalFixture(t)
			sentAt := time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC)
			messageID := f.insertMessage(t, "body-mutation", "meeting_transcript",
				f.oldConversationID, sentAt)
			if tt.seed {
				require.NoError(t, f.store.UpsertMessageBody(messageID,
					sql.NullString{String: "original body", Valid: true}, sql.NullString{}))
			}
			before := latestEmbeddingChangeSequence(t, f.store)
			tt.apply(t, f, messageID)

			changes := embeddingChangesAfter(t, f.store, before)
			require.Len(t, changes, 1)
			wantKind := store.EmbeddingChangeMessageBody
			if tt.name == "insert" {
				wantKind = store.EmbeddingChangeMessageInsert
			}
			assert.Equal(t, wantKind, changes[0].Kind)
			oldConversationID := sql.NullInt64{Int64: f.oldConversationID, Valid: true}
			newConversationID := oldConversationID
			oldSentAt := sql.NullTime{Time: sentAt, Valid: true}
			newSentAt := oldSentAt
			if tt.name == "insert" {
				oldConversationID = sql.NullInt64{}
				oldSentAt = sql.NullTime{}
			}
			if tt.name == "delete" {
				newConversationID = sql.NullInt64{}
				newSentAt = sql.NullTime{}
			}
			requireJournalScope(t, changes[0], messageID,
				oldConversationID, newConversationID, oldSentAt, newSentAt)
			oldType := sql.NullString{String: "meeting_transcript", Valid: true}
			newType := oldType
			if tt.name == "insert" {
				oldType = sql.NullString{}
			}
			if tt.name == "delete" {
				newType = sql.NullString{}
			}
			requireJournalTypes(t, changes[0], oldType, newType)
		})
	}
}

func TestEmbeddingChangeJournal_BeeperConversationMetadata(t *testing.T) {
	tests := []struct {
		name          string
		seed          func(t *testing.T, f embeddingJournalFixture)
		apply         func(t *testing.T, f embeddingJournalFixture)
		kind          store.EmbeddingChangeKind
		participantID func(f embeddingJournalFixture) sql.NullInt64
	}{
		{
			name: "title",
			apply: func(t *testing.T, f embeddingJournalFixture) {
				t.Helper()
				_, err := f.store.DB().Exec(f.store.Rebind(
					`UPDATE conversations SET title = ? WHERE id = ?`),
					"Renamed chat", f.oldConversationID)
				require.NoError(t, err)
			},
			kind: store.EmbeddingChangeConversationTitle,
		},
		{
			name: "membership insert",
			apply: func(t *testing.T, f embeddingJournalFixture) {
				t.Helper()
				require.NoError(t, f.store.EnsureConversationParticipant(
					f.oldConversationID, f.bobID, "member"))
			},
			kind: store.EmbeddingChangeConversationParticipant,
			participantID: func(f embeddingJournalFixture) sql.NullInt64 {
				return sql.NullInt64{Int64: f.bobID, Valid: true}
			},
		},
		{
			name: "membership update",
			seed: func(t *testing.T, f embeddingJournalFixture) {
				t.Helper()
				require.NoError(t, f.store.EnsureConversationParticipant(
					f.oldConversationID, f.bobID, "member"))
			},
			apply: func(t *testing.T, f embeddingJournalFixture) {
				t.Helper()
				_, err := f.store.DB().Exec(f.store.Rebind(
					`UPDATE conversation_participants SET role = ?
					 WHERE conversation_id = ? AND participant_id = ?`),
					"admin", f.oldConversationID, f.bobID)
				require.NoError(t, err)
			},
			kind: store.EmbeddingChangeConversationParticipant,
			participantID: func(f embeddingJournalFixture) sql.NullInt64 {
				return sql.NullInt64{Int64: f.bobID, Valid: true}
			},
		},
		{
			name: "membership delete",
			seed: func(t *testing.T, f embeddingJournalFixture) {
				t.Helper()
				require.NoError(t, f.store.EnsureConversationParticipant(
					f.oldConversationID, f.bobID, "member"))
			},
			apply: func(t *testing.T, f embeddingJournalFixture) {
				t.Helper()
				_, err := f.store.DB().Exec(f.store.Rebind(
					`DELETE FROM conversation_participants
					 WHERE conversation_id = ? AND participant_id = ?`),
					f.oldConversationID, f.bobID)
				require.NoError(t, err)
			},
			kind: store.EmbeddingChangeConversationParticipant,
			participantID: func(f embeddingJournalFixture) sql.NullInt64 {
				return sql.NullInt64{Int64: f.bobID, Valid: true}
			},
		},
		{
			name: "participant display name",
			seed: func(t *testing.T, f embeddingJournalFixture) {
				t.Helper()
				require.NoError(t, f.store.EnsureConversationParticipant(
					f.oldConversationID, f.aliceID, "member"))
			},
			apply: func(t *testing.T, f embeddingJournalFixture) {
				t.Helper()
				_, err := f.store.DB().Exec(f.store.Rebind(
					`UPDATE participants SET display_name = ? WHERE id = ?`),
					"Alice Renamed", f.aliceID)
				require.NoError(t, err)
			},
			kind: store.EmbeddingChangeParticipantDisplayName,
			participantID: func(f embeddingJournalFixture) sql.NullInt64 {
				return sql.NullInt64{Int64: f.aliceID, Valid: true}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			f := newEmbeddingJournalFixture(t)
			messageID := f.insertMessage(t, "metadata-message", "beeper", f.oldConversationID,
				time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
			f.materializeBody(t, messageID, "metadata body")
			if tt.seed != nil {
				tt.seed(t, f)
			}
			before := latestEmbeddingChangeSequence(t, f.store)
			tt.apply(t, f)

			changes := embeddingChangesAfter(t, f.store, before)
			require.Len(t, changes, 1)
			assert.Equal(tt.kind, changes[0].Kind)
			wantConversation := sql.NullInt64{Int64: f.oldConversationID, Valid: true}
			if tt.kind == store.EmbeddingChangeParticipantDisplayName {
				// The participant ID is the durable lookup coordinate. One display-name
				// change can affect several conversations, so the journal records one
				// participant event rather than allocating a non-atomic sequence range.
				wantConversation = sql.NullInt64{}
			}
			assert.Equal(wantConversation, changes[0].OldConversationID)
			assert.Equal(wantConversation, changes[0].NewConversationID)
			assert.False(changes[0].OldMessageType.Valid)
			assert.False(changes[0].NewMessageType.Valid)
			if tt.participantID != nil {
				assert.Equal(tt.participantID(f), changes[0].ParticipantID)
			}
		})
	}
}

func TestEmbeddingChangeJournal_ParticipantDisplayNameIncludesSenderWithoutMembership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	messageID := f.insertMessage(t, "sender-without-membership", "beeper", f.oldConversationID,
		time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
	f.materializeBody(t, messageID, "sender body")

	var membershipCount int
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(
		`SELECT COUNT(*) FROM conversation_participants
		 WHERE conversation_id = ? AND participant_id = ?`),
		f.oldConversationID, f.aliceID).Scan(&membershipCount))
	require.Zero(membershipCount, "the regression requires sender use without membership")
	before := latestEmbeddingChangeSequence(t, f.store)

	_, err := f.store.DB().Exec(f.store.Rebind(
		`UPDATE participants SET display_name = ? WHERE id = ?`), "Alice Sender", f.aliceID)
	require.NoError(err)

	changes := embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 1)
	assert.Equal(store.EmbeddingChangeParticipantDisplayName, changes[0].Kind)
	assert.Equal(sql.NullInt64{Int64: f.aliceID, Valid: true}, changes[0].ParticipantID)
}

func TestEmbeddingChangeJournal_ParticipantEffectiveDisplayFallbacks(t *testing.T) {
	tests := []struct {
		name       string
		seed       string
		update     string
		wantChange bool
	}{
		{
			name: "email fallback changes",
			seed: `UPDATE participants SET display_name = NULL, email_address = 'alice-old@example.test',
			       phone_number = '+15550001' WHERE id = ?`,
			update:     `UPDATE participants SET email_address = 'alice-new@example.test' WHERE id = ?`,
			wantChange: true,
		},
		{
			name: "phone fallback changes",
			seed: `UPDATE participants SET display_name = NULL, email_address = NULL,
			       phone_number = '+15550001' WHERE id = ?`,
			update:     `UPDATE participants SET phone_number = '+15550002' WHERE id = ?`,
			wantChange: true,
		},
		{
			name: "display name hides fallback change",
			seed: `UPDATE participants SET display_name = 'Visible Name',
			       email_address = 'alice-old@example.test', phone_number = '+15550001' WHERE id = ?`,
			update:     `UPDATE participants SET email_address = 'alice-new@example.test' WHERE id = ?`,
			wantChange: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newEmbeddingJournalFixture(t)
			messageID := f.insertMessage(t, "participant-fallback", "beeper", f.oldConversationID,
				time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
			f.materializeBody(t, messageID, "fallback body")
			_, err := f.store.DB().Exec(f.store.Rebind(tt.seed), f.aliceID)
			require.NoError(err)
			before := latestEmbeddingChangeSequence(t, f.store)

			_, err = f.store.DB().Exec(f.store.Rebind(tt.update), f.aliceID)
			require.NoError(err)
			changes := embeddingChangesAfter(t, f.store, before)
			if !tt.wantChange {
				assert.Empty(changes)
				return
			}
			require.Len(changes, 1)
			assert.Equal(store.EmbeddingChangeParticipantDisplayName, changes[0].Kind)
			assert.Equal(sql.NullInt64{Int64: f.aliceID, Valid: true}, changes[0].ParticipantID)
		})
	}
}

func TestEmbeddingChangeJournal_NoOpConversationTitleDoesNotJournal(t *testing.T) {
	f := newEmbeddingJournalFixture(t)
	messageID := f.insertMessage(t, "no-op-title", "beeper", f.oldConversationID,
		time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
	f.materializeBody(t, messageID, "title body")
	before := latestEmbeddingChangeSequence(t, f.store)

	_, err := f.store.DB().Exec(f.store.Rebind(
		`UPDATE conversations SET title = ? WHERE id = ?`), "Old chat", f.oldConversationID)
	require.NoError(t, err)

	assert.Empty(t, embeddingChangesAfter(t, f.store, before))
	assert.Equal(t, before, latestEmbeddingChangeSequence(t, f.store))
}

func TestEmbeddingChangeJournal_ContextualSubjectAndSnippetDoNotJournal(t *testing.T) {
	for _, messageType := range []string{"beeper", "meeting_transcript"} {
		for _, field := range []string{"subject", "snippet"} {
			t.Run(messageType+"/"+field, func(t *testing.T) {
				f := newEmbeddingJournalFixture(t)
				messageID := f.insertMessage(t, "ignored-"+messageType+"-"+field,
					messageType, f.oldConversationID,
					time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
				f.materializeBody(t, messageID, "renderer body")
				before := latestEmbeddingChangeSequence(t, f.store)

				statement := `UPDATE messages SET subject = ? WHERE id = ?`
				if field == "snippet" {
					statement = `UPDATE messages SET snippet = ? WHERE id = ?`
				}
				_, err := f.store.DB().Exec(f.store.Rebind(statement), "ignored change", messageID)
				require.NoError(t, err)

				assert.Empty(t, embeddingChangesAfter(t, f.store, before))
				assert.Equal(t, before, latestEmbeddingChangeSequence(t, f.store))
			})
		}
	}
}

func TestEmbeddingChangeJournal_OrdinaryEmbeddingContentMutations(t *testing.T) {
	tests := []struct {
		name     string
		seedBody bool
		apply    func(*testing.T, embeddingJournalFixture, int64)
		wantKind store.EmbeddingChangeKind
	}{
		{
			name: "body insert",
			apply: func(t *testing.T, f embeddingJournalFixture, messageID int64) {
				t.Helper()
				require.NoError(t, f.store.UpsertMessageBody(messageID,
					sql.NullString{String: "inserted ordinary body", Valid: true}, sql.NullString{}))
			},
			wantKind: store.EmbeddingChangeMessageInsert,
		},
		{
			name:     "body update",
			seedBody: true,
			apply: func(t *testing.T, f embeddingJournalFixture, messageID int64) {
				t.Helper()
				require.NoError(t, f.store.UpsertMessageBody(messageID,
					sql.NullString{String: "updated ordinary body", Valid: true}, sql.NullString{}))
			},
			wantKind: store.EmbeddingChangeMessageBody,
		},
		{
			name:     "subject update",
			seedBody: true,
			apply: func(t *testing.T, f embeddingJournalFixture, messageID int64) {
				t.Helper()
				_, err := f.store.DB().Exec(f.store.Rebind(
					`UPDATE messages SET subject = ? WHERE id = ?`), "updated ordinary subject", messageID)
				require.NoError(t, err)
			},
			wantKind: store.EmbeddingChangeMessageUpdate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newEmbeddingJournalFixture(t)
			messageID := f.insertMessage(t, "ordinary-content", "email", f.oldConversationID,
				time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
			if tt.seedBody {
				f.materializeBody(t, messageID, "original ordinary body")
			}
			before := latestEmbeddingChangeSequence(t, f.store)

			tt.apply(t, f, messageID)

			changes := embeddingChangesAfter(t, f.store, before)
			require.Len(changes, 1)
			assert.Equal(tt.wantKind, changes[0].Kind)
			assert.Equal(sql.NullInt64{Int64: messageID, Valid: true}, changes[0].MessageID)
		})
	}
}

func TestEmbeddingChangeJournal_EmailOnlyMetadataDoesNotJournal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	messageID := f.insertMessage(t, "ordinary-email", "email", f.oldConversationID,
		time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
	f.materializeBody(t, messageID, "email body")
	require.NoError(f.store.EnsureConversationParticipant(
		f.oldConversationID, f.aliceID, "member"))
	before := latestEmbeddingChangeSequence(t, f.store)

	_, err := f.store.DB().Exec(f.store.Rebind(
		`UPDATE conversations SET title = ? WHERE id = ?`), "changed", f.oldConversationID)
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(
		`UPDATE conversation_participants SET role = ?
		 WHERE conversation_id = ? AND participant_id = ?`),
		"admin", f.oldConversationID, f.aliceID)
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(
		`UPDATE participants SET display_name = ? WHERE id = ?`), "Alice Renamed", f.aliceID)
	require.NoError(err)

	assert.Empty(embeddingChangesAfter(t, f.store, before))
	assert.Equal(before, latestEmbeddingChangeSequence(t, f.store))
}

func TestEmbeddingChangeJournal_RollbackRestoresClockAndEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	messageID := f.insertMessage(t, "rollback-message", "beeper", f.oldConversationID,
		time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
	f.materializeBody(t, messageID, "rollback body")
	before := latestEmbeddingChangeSequence(t, f.store)

	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	_, err = tx.ExecContext(t.Context(), f.store.Rebind(
		`UPDATE messages SET sender_id = ? WHERE id = ?`), f.bobID, messageID)
	require.NoError(err)
	var clockInTransaction int64
	require.NoError(tx.QueryRowContext(t.Context(),
		`SELECT sequence FROM embedding_change_clock WHERE singleton = 1`).Scan(&clockInTransaction))
	assert.Equal(before+1, clockInTransaction)
	var eventsInTransaction int
	require.NoError(tx.QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM embedding_changes WHERE sequence > ?`), before).
		Scan(&eventsInTransaction))
	assert.Equal(1, eventsInTransaction)
	require.NoError(tx.Rollback())

	assert.Equal(before, latestEmbeddingChangeSequence(t, f.store))
	assert.Empty(embeddingChangesAfter(t, f.store, before))
}

func TestEmbeddingChangeJournal_InitSchemaIsIdempotent(t *testing.T) {
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	require.NoError(f.store.InitSchema())
	require.NoError(f.store.InitSchema())

	var clockRows int
	require.NoError(f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM embedding_change_clock`).Scan(&clockRows))
	assert.Equal(t, 1, clockRows)
	before := latestEmbeddingChangeSequence(t, f.store)
	messageID := f.insertMessage(t, "idempotent-message", "beeper", f.oldConversationID,
		time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
	f.materializeBody(t, messageID, "idempotent body")
	changes := embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 1, "reinstalling triggers must not duplicate an event")
}

func TestEmbeddingChangeJournal_InitSchemaUpgradesPreKindCoordinateTable(t *testing.T) {
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	_, err := f.store.DB().Exec(f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "embedding_change_journal_triggers_v7")
	require.NoError(err)
	if f.store.IsPostgreSQL() {
		for _, statement := range []string{
			`DROP TRIGGER IF EXISTS trg_embedding_changes_messages ON messages`,
			`DROP TRIGGER IF EXISTS trg_embedding_changes_message_delete ON messages`,
			`DROP TRIGGER IF EXISTS trg_embedding_changes_bodies ON message_bodies`,
			`DROP TRIGGER IF EXISTS trg_embedding_changes_conversation_title ON conversations`,
			`DROP TRIGGER IF EXISTS trg_embedding_changes_membership ON conversation_participants`,
			`DROP TRIGGER IF EXISTS trg_embedding_changes_participant_display_name ON participants`,
		} {
			_, err := f.store.DB().Exec(statement)
			require.NoError(err)
		}
	} else {
		for _, trigger := range []string{
			"trg_embedding_changes_message_update",
			"trg_embedding_changes_message_delete",
			"trg_embedding_changes_body_insert",
			"trg_embedding_changes_body_update",
			"trg_embedding_changes_body_delete",
			"trg_embedding_changes_conversation_title",
			"trg_embedding_changes_membership_insert",
			"trg_embedding_changes_membership_update",
			"trg_embedding_changes_membership_delete",
			"trg_embedding_changes_participant_display_name",
		} {
			_, err := f.store.DB().Exec(`DROP TRIGGER IF EXISTS ` + trigger)
			require.NoError(err)
		}
	}
	_, err = f.store.DB().Exec(`ALTER TABLE embedding_changes DROP COLUMN old_message_type`)
	require.NoError(err)
	_, err = f.store.DB().Exec(`ALTER TABLE embedding_changes DROP COLUMN new_message_type`)
	require.NoError(err)

	require.NoError(f.store.InitSchema())
	if !f.store.IsPostgreSQL() {
		var triggerCount int
		require.NoError(f.store.DB().QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'trg_embedding_changes_body_insert'`).Scan(&triggerCount))
		assert.Equal(t, 1, triggerCount)
	}
	id := f.insertMessage(t, "upgraded-kind-coordinate", "beeper", f.oldConversationID,
		time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
	before := latestEmbeddingChangeSequence(t, f.store)
	f.materializeBody(t, id, "upgraded body")
	changes := embeddingChangesAfter(t, f.store, before)
	require.Len(changes, 1)
	requireJournalTypes(t, changes[0], sql.NullString{},
		sql.NullString{String: "beeper", Valid: true})
}

func TestEmbeddingChangeJournal_PostgresClockSerializesCommitOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL-only row-lock and commit-order test")
	}
	sentAt := time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC)
	firstMessageID := f.insertMessage(t, "commit-first", "beeper", f.oldConversationID, sentAt)
	secondMessageID := f.insertMessage(t, "commit-second", "beeper", f.oldConversationID, sentAt)
	f.materializeBody(t, firstMessageID, "first body")
	f.materializeBody(t, secondMessageID, "second body")
	before := latestEmbeddingChangeSequence(t, f.store)

	first, err := f.store.DB().BeginTx(context.Background(), nil)
	require.NoError(err)
	firstOpen := true
	t.Cleanup(func() {
		if firstOpen {
			_ = first.Rollback()
		}
	})
	second, err := f.store.DB().BeginTx(context.Background(), nil)
	require.NoError(err)
	secondOpen := true
	t.Cleanup(func() {
		if secondOpen {
			_ = second.Rollback()
		}
	})
	var firstPID, secondPID int
	require.NoError(first.QueryRow(`SELECT pg_backend_pid()`).Scan(&firstPID))
	require.NoError(second.QueryRow(`SELECT pg_backend_pid()`).Scan(&secondPID))

	_, err = first.Exec(f.store.Rebind(
		`UPDATE messages SET sender_id = ? WHERE id = ?`), f.bobID, firstMessageID)
	require.NoError(err)
	secondUpdate := make(chan error, 1)
	go func() {
		_, updateErr := second.Exec(f.store.Rebind(
			`UPDATE messages SET sender_id = ? WHERE id = ?`), f.bobID, secondMessageID)
		secondUpdate <- updateErr
	}()

	require.Eventually(func() bool {
		var blocked bool
		queryErr := f.store.DB().QueryRow(`
			SELECT $1 = ANY(pg_blocking_pids($2))`, firstPID, secondPID).Scan(&blocked)
		return queryErr == nil && blocked
	}, 3*time.Second, 10*time.Millisecond,
		"the second journal writer must wait for the singleton clock row")

	require.NoError(first.Commit())
	firstOpen = false
	require.NoError(<-secondUpdate)

	firstPage, err := f.store.ScanEmbeddingChanges(t.Context(), before, 10)
	require.NoError(err)
	require.Len(firstPage, 1)
	assert.Equal(firstMessageID, firstPage[0].MessageID.Int64)
	assert.Equal(before+1, firstPage[0].Sequence)

	require.NoError(second.Commit())
	secondOpen = false
	secondPage, err := f.store.ScanEmbeddingChanges(t.Context(), firstPage[0].Sequence, 10)
	require.NoError(err)
	require.Len(secondPage, 1)
	assert.Equal(secondMessageID, secondPage[0].MessageID.Int64)
	assert.Equal(before+2, secondPage[0].Sequence)
}

func TestEmbeddingChangeJournal_PostgresMutationLocksClockBeforeSourceRow(t *testing.T) {
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL-only pre-mutation lock-order test")
	}
	messageID := f.insertMessage(t, "activation-race", "beeper", f.oldConversationID,
		time.Date(2026, 8, 8, 9, 30, 0, 0, time.UTC))
	f.materializeBody(t, messageID, "activation race body")

	activation, err := f.store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	activationOpen := true
	t.Cleanup(func() {
		if activationOpen {
			_ = activation.Rollback()
		}
	})
	var activationPID int
	require.NoError(activation.QueryRow(`SELECT pg_backend_pid()`).Scan(&activationPID))
	_, err = activation.Exec(`SELECT pg_advisory_xact_lock(
		hashtextextended('msgvault.embedding_change_clock', 0))`)
	require.NoError(err)
	var sequence int64
	require.NoError(activation.QueryRow(
		`SELECT sequence FROM embedding_change_clock WHERE singleton = 1 FOR UPDATE`).Scan(&sequence))

	mutation, err := f.store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	mutationOpen := true
	t.Cleanup(func() {
		if mutationOpen {
			_ = mutation.Rollback()
		}
	})
	var mutationPID int
	require.NoError(mutation.QueryRow(`SELECT pg_backend_pid()`).Scan(&mutationPID))
	mutationDone := make(chan error, 1)
	go func() {
		_, updateErr := mutation.Exec(f.store.Rebind(
			`UPDATE messages SET sender_id = ? WHERE id = ?`), f.bobID, messageID)
		mutationDone <- updateErr
	}()

	require.Eventually(func() bool {
		var blocked bool
		queryErr := f.store.DB().QueryRow(`
			SELECT $1 = ANY(pg_blocking_pids($2))`, activationPID, mutationPID).Scan(&blocked)
		return queryErr == nil && blocked
	}, 3*time.Second, 10*time.Millisecond,
		"the source mutation must wait for the activation clock lock")
	var lockedMessageID int64
	require.NoError(activation.QueryRow(`SELECT id FROM messages WHERE id = $1 FOR UPDATE NOWAIT`,
		messageID).Scan(&lockedMessageID),
		"the mutation must acquire the clock lock before it modifies or locks the source row")
	assert.Equal(t, messageID, lockedMessageID)

	require.NoError(activation.Rollback())
	activationOpen = false
	require.NoError(<-mutationDone)
	require.NoError(mutation.Commit())
	mutationOpen = false
}

func TestEmbeddingChangeJournal_PostgresInsertLocksClockBeforeActivationSnapshot(t *testing.T) {
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL-only insert-versus-activation lock test")
	}

	activation, err := f.store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	activationOpen := true
	t.Cleanup(func() {
		if activationOpen {
			_ = activation.Rollback()
		}
	})
	var activationPID int
	require.NoError(activation.QueryRow(`SELECT pg_backend_pid()`).Scan(&activationPID))
	_, err = activation.Exec(`SELECT pg_advisory_xact_lock(
		hashtextextended('msgvault.embedding_change_clock', 0))`)
	require.NoError(err)
	var sequence int64
	require.NoError(activation.QueryRow(
		`SELECT sequence FROM embedding_change_clock WHERE singleton = 1 FOR UPDATE`).Scan(&sequence))

	mutation, err := f.store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	mutationOpen := true
	t.Cleanup(func() {
		if mutationOpen {
			_ = mutation.Rollback()
		}
	})
	var mutationPID int
	require.NoError(mutation.QueryRow(`SELECT pg_backend_pid()`).Scan(&mutationPID))
	insertDone := make(chan error, 1)
	go func() {
		_, insertErr := mutation.Exec(`
			INSERT INTO messages (
				conversation_id, source_id, source_message_id, message_type
			) VALUES ($1, $2, $3, 'beeper')`,
			f.oldConversationID, f.sourceID, "activation-insert-race")
		insertDone <- insertErr
	}()

	require.Eventually(func() bool {
		var blocked bool
		queryErr := f.store.DB().QueryRow(`
			SELECT $1 = ANY(pg_blocking_pids($2))`, activationPID, mutationPID).Scan(&blocked)
		return queryErr == nil && blocked
	}, 3*time.Second, 10*time.Millisecond,
		"message insertion must wait for the activation clock lock")

	require.NoError(activation.Rollback())
	activationOpen = false
	require.NoError(<-insertDone)
	require.NoError(mutation.Commit())
	mutationOpen = false
}

func TestEmbeddingChangeJournal_ScanPagesInSequenceOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingJournalFixture(t)
	before := latestEmbeddingChangeSequence(t, f.store)
	for i := range 3 {
		messageID := f.insertMessage(t, fmt.Sprintf("page-%d", i), "beeper", f.oldConversationID,
			time.Date(2026, 8, 8, 9, i, 0, 0, time.UTC))
		f.materializeBody(t, messageID, fmt.Sprintf("body %d", i))
	}

	page, err := f.store.ScanEmbeddingChanges(t.Context(), before, 2)
	require.NoError(err)
	require.Len(page, 2)
	assert.Equal(before+2, page[0].Sequence)
	assert.Equal(before+4, page[1].Sequence)
	next, err := f.store.ScanEmbeddingChanges(t.Context(), page[1].Sequence, 2)
	require.NoError(err)
	require.Len(next, 1)
	assert.Equal(before+6, next[0].Sequence)
}
