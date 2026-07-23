package store_test

import (
	"database/sql"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type failingMessageIDReader struct{}

func (failingMessageIDReader) Read([]byte) (int, error) {
	return 0, errors.New("synthetic staged-ID read failure")
}

func TestRecomputeConversationStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("whatsapp", "+15550000001")
	require.NoError(err, "GetOrCreateSource")

	convID, err := st.EnsureConversationWithType(source.ID, "conv-1", "whatsapp_dm", "Test Chat")
	require.NoError(err, "EnsureConversationWithType")

	// Verify initial message_count is 0 (stats not maintained on insert).
	var initialCount int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT message_count FROM conversations WHERE id = ?`), convID,
	).Scan(&initialCount), "initial message_count scan")
	assert.Equal(0, initialCount, "initial message_count")

	sentAt := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	msg1 := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "msg-1",
		ConversationID:  convID,
		MessageType:     "whatsapp",
		SentAt:          sql.NullTime{Time: sentAt, Valid: true},
		Snippet:         sql.NullString{String: "hello", Valid: true},
	}
	_, err = st.UpsertMessage(msg1)
	require.NoError(err, "UpsertMessage msg1")

	sentAt2 := sentAt.Add(time.Hour)
	msg2 := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "msg-2",
		ConversationID:  convID,
		MessageType:     "whatsapp",
		SentAt:          sql.NullTime{Time: sentAt2, Valid: true},
		Snippet:         sql.NullString{String: "world", Valid: true},
	}
	_, err = st.UpsertMessage(msg2)
	require.NoError(err, "UpsertMessage msg2")

	// msg3 has the SAME sent_at as msg2 but a different snippet.
	// After recompute, last_message_preview must come from msg3 (higher id),
	// exercising the `id DESC` tie-breaker in the SQL.
	msg3 := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "msg-3",
		ConversationID:  convID,
		MessageType:     "whatsapp",
		SentAt:          sql.NullTime{Time: sentAt2, Valid: true},
		Snippet:         sql.NullString{String: "tie-breaker", Valid: true},
	}
	_, err = st.UpsertMessage(msg3)
	require.NoError(err, "UpsertMessage msg3")

	// Add a conversation participant so participant_count is non-zero.
	participantID, err := st.EnsureParticipantByPhone("+15559876543", "Bob", "whatsapp")
	require.NoError(err, "EnsureParticipantByPhone")
	require.NoError(st.EnsureConversationParticipant(convID, participantID, "member"),
		"EnsureConversationParticipant")

	// Recompute and verify counts.
	require.NoError(st.RecomputeConversationStats(source.ID), "RecomputeConversationStats")

	var count int
	var participantCount int
	var lastMsgAt sql.NullTime
	var preview sql.NullString
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT message_count, participant_count, last_message_at, last_message_preview
		 FROM conversations WHERE id = ?`), convID,
	).Scan(&count, &participantCount, &lastMsgAt, &preview), "post-recompute scan")
	assert.Equal(3, count, "message_count")
	assert.Equal(1, participantCount, "participant_count")
	assert.True(lastMsgAt.Valid, "last_message_at should not be NULL")
	// msg2 and msg3 share the same sent_at; msg3 has the higher id, so its
	// snippet ("tie-breaker") must win via the `id DESC` tie-breaker.
	assert.True(preview.Valid, "preview valid")
	assert.Equal("tie-breaker", preview.String, "last_message_preview")

	// Idempotency: calling again should produce the same result.
	require.NoError(st.RecomputeConversationStats(source.ID), "RecomputeConversationStats (second call)")
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT message_count FROM conversations WHERE id = ?`), convID,
	).Scan(&count), "idempotency scan")
	assert.Equal(3, count, "idempotency message_count")
}

// TestEmbedGen_OrphanImpossibleAndCoverage pins the scan-and-fill
// embed_gen contract:
//   - a freshly-upserted message has embed_gen NULL (column default), so
//     CoverageCounts reports it as missing for any generation — the
//     scan-and-fill worker picks it up with no enqueue step (orphan rows
//     are impossible).
//   - SetEmbedGen stamps it covered; CoverageCounts then reports it
//     embedded.
//   - a subsequent UpsertMessage (ON CONFLICT DO UPDATE) clears embed_gen
//     when the embeddable subject text changes.
func TestEmbedGen_OrphanImpossibleAndCoverage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "me@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(source.ID, "conv-1", "email_thread", "Subject")
	require.NoError(err, "EnsureConversationWithType")

	msg := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "m1",
		ConversationID:  convID,
		MessageType:     "email",
		Subject:         sql.NullString{String: "hello", Valid: true},
	}
	id, err := st.UpsertMessage(msg)
	require.NoError(err, "UpsertMessage")

	const gen = int64(7)
	ctx := t.Context()

	// New row: embed_gen NULL by default -> reported missing for any gen.
	var embedGen sql.NullInt64
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT embed_gen FROM messages WHERE id = ?`), id).Scan(&embedGen))
	assert.False(embedGen.Valid, "new message must have NULL embed_gen (no enqueue, no orphan)")

	live, embedded, _, missing, err := st.CoverageCounts(ctx, gen)
	require.NoError(err, "CoverageCounts (before stamp)")
	assert.Equal(int64(1), live, "one live message")
	assert.Equal(int64(0), embedded, "none embedded yet")
	assert.Equal(int64(1), missing, "the new message is missing")

	// Stamp it covered.
	require.NoError(st.SetEmbedGen(ctx, []int64{id}, gen), "SetEmbedGen")
	live, embedded, _, missing, err = st.CoverageCounts(ctx, gen)
	require.NoError(err, "CoverageCounts (after stamp)")
	assert.Equal(int64(1), live, "still one live message")
	assert.Equal(int64(1), embedded, "now embedded")
	assert.Equal(int64(0), missing, "nothing missing")

	// Re-upsert the same message with changed embedding input: embed_gen must
	// be cleared so the scan-and-fill worker re-embeds it.
	msg.Subject = sql.NullString{String: "hello (edited)", Valid: true}
	_, err = st.UpsertMessage(msg)
	require.NoError(err, "re-UpsertMessage")
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT embed_gen FROM messages WHERE id = ?`), id).Scan(&embedGen))
	assert.False(embedGen.Valid, "subject change must clear embed_gen")
}

func TestMigrateSourceMessageIDRepointsRepliesBeforeDeletingDuplicate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("teams", "user@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(source.ID, "team-1/channel-1", "channel", "General")
	require.NoError(err, "EnsureConversationWithType")

	legacyParentID := insertStoreTestMessage(t, st, source.ID, convID, "m1")
	scopedParentID := insertStoreTestMessage(t, st, source.ID, convID, "channel:team-1:channel-1:m1")
	childID := insertStoreTestMessage(t, st, source.ID, convID, "m2")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET reply_to_message_id = ? WHERE id = ?`),
		legacyParentID, childID,
	)
	require.NoError(err, "seed reply")

	require.NoError(
		st.MigrateSourceMessageID(source.ID, convID, "m1", "channel:team-1:channel-1:m1"),
		"MigrateSourceMessageID",
	)

	var replyTo sql.NullInt64
	err = st.DB().QueryRow(
		st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`),
		childID,
	).Scan(&replyTo)
	require.NoError(err, "scan reply_to_message_id")
	require.True(replyTo.Valid, "reply_to_message_id should remain set")
	assert.Equal(scopedParentID, replyTo.Int64, "reply should point at scoped parent")

	var legacyCount int
	err = st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM messages WHERE id = ?`),
		legacyParentID,
	).Scan(&legacyCount)
	require.NoError(err, "legacy count")
	assert.Equal(0, legacyCount, "legacy duplicate should be deleted")
}

func TestListUnresolvedMessageRepliesReturnsOnlyUnlinkedProviderMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "guild-1")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(source.ID, "channel-1", "channel", "General")
	require.NoError(err)

	parentID := insertStoreTestMessage(t, st, source.ID, convID, "100")
	childID := insertStoreTestMessage(t, st, source.ID, convID, "101")
	linkedID := insertStoreTestMessage(t, st, source.ID, convID, "102")
	nonReplyID := insertStoreTestMessage(t, st, source.ID, convID, "103")
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET message_type = 'discord' WHERE source_id = ?`), source.ID)
	require.NoError(err)
	require.NoError(st.SetMessageMetadata(childID, sql.NullString{
		String: `{"referenced_message_id":"100","referenced_channel_id":"channel-1"}`, Valid: true,
	}))
	require.NoError(st.SetMessageMetadata(linkedID, sql.NullString{
		String: `{"referenced_message_id":"100"}`, Valid: true,
	}))
	require.NoError(st.SetMessageMetadata(nonReplyID, sql.NullString{
		String: `{"reaction_summaries":[{"emoji":"thumbsup","count":1}]}`, Valid: true,
	}))
	require.NoError(st.SetReplyTo(source.ID, "102", "100"))

	unresolved, err := st.ListUnresolvedMessageReplies(source.ID, "discord")
	require.NoError(err)
	require.Len(unresolved, 1)
	assert.Equal(childID, unresolved[0].MessageID)
	assert.Equal("101", unresolved[0].SourceMessageID)
	assert.JSONEq(`{"referenced_message_id":"100","referenced_channel_id":"channel-1"}`, unresolved[0].Metadata)
	assert.NotZero(parentID)
}

func TestListUnresolvedMessageRepliesAfterUsesBoundedKeysetPages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "guild-keyset")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(source.ID, "channel-keyset", "channel", "General")
	require.NoError(err)

	var ids []int64
	for _, sourceMessageID := range []string{"101", "102", "103", "104", "105"} {
		messageID := insertStoreTestMessage(t, st, source.ID, convID, sourceMessageID)
		if sourceMessageID != "102" {
			ids = append(ids, messageID)
		}
		metadata := `{"referenced_message_id":"100"}`
		if sourceMessageID == "102" {
			metadata = `{"reaction_summaries":[]}`
		}
		require.NoError(st.SetMessageMetadata(messageID, sql.NullString{
			String: metadata, Valid: true,
		}))
	}
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET message_type = 'discord' WHERE source_id = ?`), source.ID)
	require.NoError(err)

	first, err := st.ListUnresolvedMessageRepliesAfter(source.ID, "discord", 0, 2)
	require.NoError(err)
	require.Len(first, 2)
	wantFirst := ids[:2]
	gotFirst := []int64{first[0].MessageID, first[1].MessageID}
	assert.Equal(wantFirst, gotFirst)
	second, err := st.ListUnresolvedMessageRepliesAfter(source.ID, "discord", first[1].MessageID, 2)
	require.NoError(err)
	require.Len(second, 2)
	wantSecond := ids[2:]
	gotSecond := []int64{second[0].MessageID, second[1].MessageID}
	assert.Equal(wantSecond, gotSecond)
	last, err := st.ListUnresolvedMessageRepliesAfter(source.ID, "discord", second[1].MessageID, 2)
	require.NoError(err)
	assert.Empty(last)
}

func TestMigrateSourceMessageIDClearsTombstoneWhenRenamingLegacyRow(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("teams", "user@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(source.ID, "19:x@thread.v2", "direct_chat", "DM")
	require.NoError(err, "EnsureConversationWithType")
	_ = insertStoreTestMessage(t, st, source.ID, convID, "m1")
	require.NoError(st.MarkMessageDeleted(source.ID, "m1"), "MarkMessageDeleted")

	require.NoError(
		st.MigrateSourceMessageID(source.ID, convID, "m1", "chat:19:x@thread.v2:m1"),
		"MigrateSourceMessageID",
	)

	assertSourceMessageIDNotDeleted(t, st, source.ID, "chat:19:x@thread.v2:m1")
}

func TestMigrateSourceMessageIDClearsTombstoneOnExistingScopedRow(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("teams", "user@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(source.ID, "19:x@thread.v2", "direct_chat", "DM")
	require.NoError(err, "EnsureConversationWithType")
	_ = insertStoreTestMessage(t, st, source.ID, convID, "chat:19:x@thread.v2:m1")
	require.NoError(st.MarkMessageDeleted(source.ID, "chat:19:x@thread.v2:m1"), "MarkMessageDeleted")

	require.NoError(
		st.MigrateSourceMessageID(source.ID, convID, "m1", "chat:19:x@thread.v2:m1"),
		"MigrateSourceMessageID",
	)

	assertSourceMessageIDNotDeleted(t, st, source.ID, "chat:19:x@thread.v2:m1")
}

func TestMessageSourceIDsInSnowflakeInterval(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "123456789012345678")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "234567890123456789", "channel", "general",
	)
	require.NoError(err)

	for _, sourceMessageID := range []string{
		"99",
		"100",
		"101",
		"9223372036854775807",
		"9223372036854775808",
		"10000000000000000000",
		"1000000000000000000a",
		"09223372036854775808",
		"18446744073709551615",
		"18446744073709551616",
	} {
		insertStoreTestMessage(t, st, source.ID, conversationID, sourceMessageID)
	}

	otherConversationID, err := st.EnsureConversationWithType(
		source.ID, "234567890123456790", "channel", "random",
	)
	require.NoError(err)
	insertStoreTestMessage(t, st, source.ID, otherConversationID, "10000000000000000001")

	otherSource, err := st.GetOrCreateSource("discord", "999999999999999999")
	require.NoError(err)
	otherSourceConversationID, err := st.EnsureConversationWithType(
		otherSource.ID, "234567890123456789", "channel", "general",
	)
	require.NoError(err)
	insertStoreTestMessage(t, st, otherSource.ID, otherSourceConversationID, "10000000000000000002")

	got, err := st.MessageSourceIDsInSnowflakeInterval(
		source.ID, conversationID, "9223372036854775807", "18446744073709551615",
	)
	require.NoError(err)
	assert.Equal([]string{
		"9223372036854775808",
		"10000000000000000000",
		"18446744073709551615",
	}, got)

	adjacent, err := st.MessageSourceIDsInSnowflakeInterval(source.ID, conversationID, "99", "100")
	require.NoError(err)
	assert.Equal([]string{"100"}, adjacent)

	zeroPadded, err := st.MessageSourceIDsInSnowflakeInterval(
		source.ID, conversationID, "0009223372036854775807", "00018446744073709551615",
	)
	require.NoError(err)
	assert.Equal(got, zeroPadded)
}

func TestMessageSourceIDsInSnowflakeIntervalPageAndMaximum(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "paged-guild")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "paged-channel", "channel", "general")
	require.NoError(err)
	for _, sourceMessageID := range []string{"100", "101", "102", "103", "104"} {
		insertStoreTestMessage(t, st, source.ID, conversationID, sourceMessageID)
	}

	maximum, err := st.MaxMessageSourceIDInSnowflakeInterval(source.ID, conversationID, "99", "104")
	require.NoError(err)
	assert.Equal("104", maximum)
	first, err := st.MessageSourceIDsInSnowflakeIntervalPage(
		source.ID, conversationID, "99", "104", "", 2,
	)
	require.NoError(err)
	assert.Equal([]string{"104", "103"}, first)
	second, err := st.MessageSourceIDsInSnowflakeIntervalPage(
		source.ID, conversationID, "99", "104", first[len(first)-1], 2,
	)
	require.NoError(err)
	assert.Equal([]string{"102", "101"}, second)
	last, err := st.MessageSourceIDsInSnowflakeIntervalPage(
		source.ID, conversationID, "99", "104", second[len(second)-1], 2,
	)
	require.NoError(err)
	assert.Equal([]string{"100"}, last)
}

func TestMessageSourceIDsInSnowflakeIntervalRejectsUnsafeBounds(t *testing.T) {
	st := testutil.NewTestStore(t)

	for _, tc := range []struct {
		name  string
		lower string
		upper string
	}{
		{name: "empty lower", lower: "", upper: "100"},
		{name: "empty upper", lower: "99", upper: ""},
		{name: "malformed lower", lower: "9x", upper: "100"},
		{name: "malformed upper", lower: "99", upper: "10_0"},
		{name: "reversed", lower: "101", upper: "100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			_, err := st.MessageSourceIDsInSnowflakeInterval(1, 1, tc.lower, tc.upper)
			require.Error(err)
		})
	}
}

func TestClearMessageDeletedFromSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "123456789012345678")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "234567890123456789", "channel", "general",
	)
	require.NoError(err)
	insertStoreTestMessage(t, st, source.ID, conversationID, "345678901234567890")
	require.NoError(st.MarkMessageDeleted(source.ID, "345678901234567890"))

	otherSource, err := st.GetOrCreateSource("discord", "999999999999999999")
	require.NoError(err)
	otherConversationID, err := st.EnsureConversationWithType(
		otherSource.ID, "888888888888888888", "channel", "other",
	)
	require.NoError(err)
	insertStoreTestMessage(t, st, otherSource.ID, otherConversationID, "345678901234567890")
	require.NoError(st.MarkMessageDeleted(otherSource.ID, "345678901234567890"))

	require.NoError(st.ClearMessageDeletedFromSource(source.ID, "345678901234567890"))
	assertSourceMessageIDNotDeleted(t, st, source.ID, "345678901234567890")

	var otherDeletedAt sql.NullTime
	err = st.DB().QueryRow(
		st.Rebind(`SELECT deleted_from_source_at FROM messages WHERE source_id = ? AND source_message_id = ?`),
		otherSource.ID, "345678901234567890",
	).Scan(&otherDeletedAt)
	require.NoError(err)
	assert.True(otherDeletedAt.Valid)
}

func TestMarkMessagesDeletedFromReaderIsAtomicOnLateReadFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "atomic-delete-guild")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "atomic-channel", "channel", "general")
	require.NoError(err)
	for _, sourceMessageID := range []string{"101", "102"} {
		insertStoreTestMessage(t, st, source.ID, conversationID, sourceMessageID)
	}
	reader := io.MultiReader(strings.NewReader("101\n102\n"), failingMessageIDReader{})

	err = st.MarkMessagesDeletedFromReader(source.ID, reader, 1)
	require.ErrorContains(err, "synthetic staged-ID read failure")
	var deleted int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages
		WHERE source_id = ? AND deleted_from_source_at IS NOT NULL
	`), source.ID).Scan(&deleted))
	assert.Zero(deleted, "late reader failure rolls back earlier bounded batches")

	require.NoError(st.MarkMessagesDeletedFromReader(source.ID, strings.NewReader("101\n102\n"), 1))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages
		WHERE source_id = ? AND deleted_from_source_at IS NOT NULL
	`), source.ID).Scan(&deleted))
	assert.Equal(2, deleted)
}

func insertStoreTestMessage(t *testing.T, st *store.Store, sourceID, convID int64, sourceMessageID string) int64 {
	t.Helper()
	msg := &store.Message{
		SourceID:        sourceID,
		SourceMessageID: sourceMessageID,
		ConversationID:  convID,
		MessageType:     "teams",
		SentAt:          sql.NullTime{Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC), Valid: true},
		Snippet:         sql.NullString{String: sourceMessageID, Valid: true},
	}
	id, err := st.UpsertMessage(msg)
	require.NoError(t, err, "UpsertMessage "+sourceMessageID)
	return id
}

func assertSourceMessageIDNotDeleted(t *testing.T, st *store.Store, sourceID int64, sourceMessageID string) {
	t.Helper()
	var deletedAt sql.NullTime
	err := st.DB().QueryRow(
		st.Rebind(`SELECT deleted_from_source_at FROM messages WHERE source_id = ? AND source_message_id = ?`),
		sourceID, sourceMessageID,
	).Scan(&deletedAt)
	require.NoError(t, err, "scan deleted_from_source_at")
	assert.False(t, deletedAt.Valid, "deleted_from_source_at should be cleared")
}

func TestEnsureParticipantByPhone_IdentifierType(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Create participant via WhatsApp
	id1, err := st.EnsureParticipantByPhone("+15551234567", "Alice", "whatsapp")
	require.NoError(err, "EnsureParticipantByPhone(whatsapp)")
	require.NotZero(id1, "expected non-zero participant ID")

	// Same phone via iMessage — should return the same participant ID
	id2, err := st.EnsureParticipantByPhone("+15551234567", "Alice", "imessage")
	require.NoError(err, "EnsureParticipantByPhone(imessage)")
	assert.Equal(id1, id2, "imessage call should return same participant ID as whatsapp")

	// Both participant_identifiers rows should exist
	var count int
	err = st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM participant_identifiers WHERE participant_id = ?`),
		id1,
	).Scan(&count)
	require.NoError(err, "count participant_identifiers")
	assert.Equal(2, count, "participant_identifiers count")

	// Verify each identifier type is present
	for _, identType := range []string{"whatsapp", "imessage"} {
		var exists int
		err = st.DB().QueryRow(
			st.Rebind(`SELECT COUNT(*) FROM participant_identifiers
			 WHERE participant_id = ? AND identifier_type = ?`),
			id1, identType,
		).Scan(&exists)
		require.NoError(err, "check identifier_type %q", identType)
		assert.Equal(1, exists, "identifier_type %q", identType)
	}
}

func TestUpdateParticipantDisplayNameByEmail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Create an unnamed email participant (e.g. inserted by iMessage import
	// for an Apple ID handle).
	var pid int64
	require.NoError(st.DB().QueryRow(
		st.Rebind(`INSERT INTO participants (email_address) VALUES (?) RETURNING id`),
		"alice@example.com",
	).Scan(&pid), "insert participant")

	// Backfilling on an empty display_name succeeds.
	updated, err := st.UpdateParticipantDisplayNameByEmail("alice@example.com", "Alice Example")
	require.NoError(err, "UpdateParticipantDisplayNameByEmail")
	require.True(updated, "expected backfill to update existing participant")

	got := readDisplayName(t, st, pid)
	assert.Equal("Alice Example", got, "display_name")

	// Lookup is case-insensitive on the email.
	updatedMixed, err := st.UpdateParticipantDisplayNameByEmail("ALICE@example.com", "Should Not Overwrite")
	require.NoError(err, "UpdateParticipantDisplayNameByEmail (case)")
	assert.False(updatedMixed, "second update should not modify a non-empty display_name")
	assert.Equal("Alice Example", readDisplayName(t, st, pid), "display_name should not be overwritten")

	// Empty inputs are no-ops.
	updated, err = st.UpdateParticipantDisplayNameByEmail("", "X")
	require.NoError(err, "empty email err")
	assert.False(updated, "empty email updated")
	updated, err = st.UpdateParticipantDisplayNameByEmail("x@y.com", "")
	require.NoError(err, "empty name err")
	assert.False(updated, "empty name updated")

	// Unknown email is a no-op (does not create rows).
	updated, err = st.UpdateParticipantDisplayNameByEmail("nobody@example.com", "Nobody")
	require.NoError(err, "unknown email err")
	assert.False(updated, "unknown email updated")
}

func TestUpdateImessageParticipantDisplayNameByPhone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Case 1: legacy iMessage participant with display_name = phone_number.
	// Should be overwritten by the contact name.
	legacyID, err := st.EnsureParticipantByPhone("+15551111111", "+15551111111", "imessage")
	require.NoError(err, "seed legacy")

	// Case 2: iMessage participant already named by another source. Real
	// name must be preserved.
	namedID, err := st.EnsureParticipantByPhone("+15552222222", "Bob From Gmail", "imessage")
	require.NoError(err, "seed named")

	// Case 3: WhatsApp-only participant with display_name = phone_number.
	// Not iMessage, must NOT be touched (no imessage identifier exists).
	otherID, err := st.EnsureParticipantByPhone("+15553333333", "+15553333333", "whatsapp")
	require.NoError(err, "seed other")

	// Apply contact-name backfill.
	updated, err := st.UpdateImessageParticipantDisplayNameByPhone("+15551111111", "Alice Real")
	require.NoError(err, "backfill legacy")
	assert.True(updated, "legacy placeholder should be replaced")
	assert.Equal("Alice Real", readDisplayName(t, st, legacyID), "legacy display_name")

	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("+15552222222", "Should Not Win")
	require.NoError(err, "backfill named")
	assert.False(updated, "real name from another source should be preserved")
	assert.Equal("Bob From Gmail", readDisplayName(t, st, namedID), "named display_name")

	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("+15553333333", "Not Allowed")
	require.NoError(err, "backfill other")
	assert.False(updated, "non-iMessage participant should not be touched")
	assert.Equal("+15553333333", readDisplayName(t, st, otherID), "non-iMessage display_name")

	// Empty inputs are no-ops.
	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("", "X")
	require.NoError(err, "empty phone err")
	assert.False(updated, "empty phone updated")
	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("+15551111111", "")
	require.NoError(err, "empty name err")
	assert.False(updated, "empty name updated")
}

func TestRetitleImessageChats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err, "source")

	otherSrc, err := st.GetOrCreateSource("whatsapp", "+15550000000")
	require.NoError(err, "other source")

	// Named iMessage participant whose phone is the current title of a 1:1.
	namedID, err := st.EnsureParticipantByPhone("+15551111111", "Alice Real", "imessage")
	require.NoError(err, "seed alice")

	// Email-backed iMessage participants did not always get an iMessage
	// participant_identifiers row, but the apple_messages conversation is
	// still enough context to safely refresh the raw email title.
	emailID, err := st.EnsureParticipant("alice@example.com", "Alice Email", "example.com")
	require.NoError(err, "seed alice email")

	// iMessage participant whose name is still the phone (poisoned). Must
	// not be used as a title.
	poisonedID, err := st.EnsureParticipantByPhone("+15552222222", "+15552222222", "imessage")
	require.NoError(err, "seed poisoned")

	// Non-iMessage participant whose phone is a conversation title — must
	// not be touched even if a real name exists elsewhere.
	whatsappID, err := st.EnsureParticipantByPhone("+15553333333", "Carol", "whatsapp")
	require.NoError(err, "seed carol")

	// 1:1 with named participant — title is the phone, should be replaced.
	convNamedID, err := st.EnsureConversationWithType(src.ID, "imsg-1", "direct_chat", "+15551111111")
	require.NoError(err, "conv named")
	require.NoError(st.EnsureConversationParticipant(convNamedID, namedID, "member"), "link named")

	// 1:1 with email participant — title is the raw email, should be replaced.
	convEmailID, err := st.EnsureConversationWithType(src.ID, "imsg-email-1", "direct_chat", "alice@example.com")
	require.NoError(err, "conv email")
	require.NoError(st.EnsureConversationParticipant(convEmailID, emailID, "member"), "link email")

	// 1:1 with poisoned participant — title equals phone but participant
	// has no real name yet. Must remain unchanged.
	convPoisonedID, err := st.EnsureConversationWithType(src.ID, "imsg-2", "direct_chat", "+15552222222")
	require.NoError(err, "conv poisoned")
	require.NoError(st.EnsureConversationParticipant(convPoisonedID, poisonedID, "member"), "link poisoned")

	// Non-iMessage 1:1 — title is a phone, but the source isn't apple_messages.
	convOtherID, err := st.EnsureConversationWithType(otherSrc.ID, "wa-1", "direct_chat", "+15553333333")
	require.NoError(err, "conv other")
	require.NoError(st.EnsureConversationParticipant(convOtherID, whatsappID, "member"), "link other")

	// Group chat whose title was generated from raw participant handles
	// before contacts were backfilled. It should be regenerated with names.
	bobID, err := st.EnsureParticipantByPhone("+15554444444", "Bob Real", "imessage")
	require.NoError(err, "seed bob")
	carolID, err := st.EnsureParticipantByPhone("+15555555555", "Carol Real", "imessage")
	require.NoError(err, "seed carol")
	daveID, err := st.EnsureParticipantByPhone("+15556666666", "Dave Real", "imessage")
	require.NoError(err, "seed dave")
	convGroupID, err := st.EnsureConversationWithType(
		src.ID, "imsg-group-1", "group_chat",
		"+15551111111, +15554444444, +15555555555 +1 more",
	)
	require.NoError(err, "conv group")
	for _, pid := range []int64{namedID, bobID, carolID, daveID} {
		require.NoError(st.EnsureConversationParticipant(convGroupID, pid, "member"),
			"link group participant %d", pid)
	}

	// Named group chats must not be overwritten, even when the participant
	// list would allow a generated title.
	convNamedGroupID, err := st.EnsureConversationWithType(
		src.ID, "imsg-group-2", "group_chat", "Road trip",
	)
	require.NoError(err, "conv named group")
	for _, pid := range []int64{namedID, bobID, carolID} {
		require.NoError(st.EnsureConversationParticipant(convNamedGroupID, pid, "member"),
			"link named group participant %d", pid)
	}

	n, err := st.RetitleImessageChats()
	require.NoError(err, "RetitleImessageChats")
	assert.Equal(int64(3), n, "rows updated")

	assert.Equal("Alice Real", readConvTitle(t, st, convNamedID), "named conv title")
	assert.Equal("Alice Email", readConvTitle(t, st, convEmailID), "email conv title")
	assert.Equal("+15552222222", readConvTitle(t, st, convPoisonedID), "poisoned conv title (unchanged)")
	assert.Equal("+15553333333", readConvTitle(t, st, convOtherID), "non-imessage conv title (unchanged)")
	assert.Equal("Alice Real, Bob Real, Carol Real +1 more",
		readConvTitle(t, st, convGroupID), "group conv title (refreshed generated title)")
	assert.Equal("Road trip", readConvTitle(t, st, convNamedGroupID), "named group conv title (unchanged)")

	// Idempotent: running again is a no-op.
	n2, err := st.RetitleImessageChats()
	require.NoError(err, "idempotent rerun err")
	assert.Equal(int64(0), n2, "idempotent rerun rows")
}

func readConvTitle(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	var title sql.NullString
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT title FROM conversations WHERE id = ?`), id,
	).Scan(&title), "scan title")
	return title.String
}

func readDisplayName(t *testing.T, st *store.Store, pid int64) string {
	t.Helper()
	var name sql.NullString
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT display_name FROM participants WHERE id = ?`), pid,
	).Scan(&name), "scan display_name")
	return name.String
}
