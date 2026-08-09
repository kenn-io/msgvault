package embed

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
)

func TestAssembleOrdinaryDocument_AlwaysFitsContextualRequestBudget(t *testing.T) {
	require := require.New(t)
	policy := AssemblyPolicy{MaxChunkRunes: 128, MaxDocumentUTF8Bytes: 220}
	doc, ok := assembleOrdinaryDocument(AssemblyMessage{
		ID: 1, ConversationID: 2, MessageType: "email",
		Body: strings.Repeat("🙂", 128), LastModified: "v1",
	}, policy)
	require.True(ok)
	require.NotEmpty(doc.Chunks)

	input := DocumentInput{Chunks: make([]string, len(doc.Chunks))}
	for i, chunk := range doc.Chunks {
		input.Chunks[i] = chunk.Text
	}
	_, err := PackDocuments([]DocumentInput{input}, RequestLimits{
		MaxDocuments: 1, MaxChunks: 16_000, MaxUTF8Bytes: policy.MaxDocumentUTF8Bytes,
	})
	require.NoError(err)
	assert.True(t, doc.Chunks[len(doc.Chunks)-1].Truncated)
}

type chatAssemblerStub struct{}

func (chatAssemblerStub) AssembleScopes(_ context.Context, snapshot SourceSnapshot, scopes []AffectedScope) ([]Document, error) {
	return []Document{{
		Key:            "chat-specialized",
		Kind:           "beeper-window",
		ScopeKey:       fmt.Sprintf("chat:%d", scopes[0].ConversationID),
		SourceSequence: snapshot.SourceSequence(),
	}}, nil
}

func TestSourceSnapshot_HoldsOneReadTransactionAndRejectsReadsAfterClose(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	id := seedAssemblyMessage(t, st, "email", "subject", "before")
	var conversationID int64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT conversation_id FROM messages WHERE id = ?"), id).Scan(&conversationID))
	participantID, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(err)
	require.NoError(st.EnsureConversationParticipant(conversationID, participantID, "member"))
	wantSequence, err := st.LatestEmbeddingChangeSequence(t.Context())
	require.NoError(err)

	snapshot, err := BeginSourceSnapshot(context.Background(), st)
	require.NoError(err)
	t.Cleanup(func() { _ = snapshot.Close() })
	assert.Equal(wantSequence, snapshot.SourceSequence())

	first, found, err := snapshot.Message(context.Background(), id)
	require.NoError(err)
	require.True(found)
	assert.Equal("before", first.Body)

	rows, err := snapshot.Messages(context.Background(), AffectedScope{
		Kind: "email", ConversationID: conversationID,
		UTCStart: time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC),
		UTCEnd:   time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(id, rows[0].ID)

	conversation, found, err := snapshot.Conversation(context.Background(), conversationID)
	require.NoError(err)
	require.True(found)
	assert.Equal("Synthetic conversation", conversation.Title)
	require.Len(conversation.Participants, 1)
	assert.Equal("Alice", conversation.Participants[0].DisplayName)
	assert.NotEmpty(conversation.MetadataVersion.Digest)

	require.NoError(st.UpsertMessageBody(id,
		sql.NullString{String: "after", Valid: true}, sql.NullString{}))
	second, found, err := snapshot.Message(context.Background(), id)
	require.NoError(err)
	require.True(found)
	assert.Equal("before", second.Body, "all source rows must come from the same SQLite read transaction")

	require.NoError(snapshot.Close())
	_, _, err = snapshot.Message(context.Background(), id)
	require.ErrorIs(err, ErrSourceSnapshotClosed)
	assert.NoError(snapshot.Close(), "closing a snapshot must be idempotent")
}

func TestSourceSnapshot_SQLiteNormalizesOffsetTimestampsForUTCDayScopes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	firstID := seedAssemblyMessage(t, st, "beeper", "", "offset message")
	var conversationID, sourceID int64
	require.NoError(st.DB().QueryRow(`SELECT conversation_id, source_id FROM messages WHERE id = ?`, firstID).
		Scan(&conversationID, &sourceID))
	_, err := st.DB().Exec(`UPDATE messages SET sent_at = '2026-08-08T23:30:00-02:00' WHERE id = ?`, firstID)
	require.NoError(err)
	secondID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: sourceID, SourceMessageID: "utc-earlier",
		MessageType: "beeper", SentAt: sql.NullTime{
			Time: time.Date(2026, 8, 9, 0, 30, 0, 0, time.UTC), Valid: true,
		},
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(secondID,
		sql.NullString{String: "earlier UTC message", Valid: true}, sql.NullString{}))

	snapshot, err := BeginSourceSnapshot(t.Context(), st)
	require.NoError(err)
	t.Cleanup(func() { _ = snapshot.Close() })
	augustEight, err := snapshot.Messages(t.Context(), AffectedScope{
		Kind: "beeper", ConversationID: conversationID,
		UTCStart: time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC),
		UTCEnd:   time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.Empty(augustEight)
	augustNine, err := snapshot.Messages(t.Context(), AffectedScope{
		Kind: "beeper", ConversationID: conversationID,
		UTCStart: time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC),
		UTCEnd:   time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	require.Len(augustNine, 2)
	assert.Equal(secondID, augustNine[0].ID)
	assert.Equal(firstID, augustNine[1].ID)
}

func TestCompositeAssembler_RoutesSpecializedTypesAndKeepsOrdinaryBehavior(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	emailID := seedAssemblyMessage(t, st, "email", "Roadmap", "first second third fourth fifth sixth")
	smsID := seedAssemblyMessage(t, st, "sms", "", "ordinary SMS")
	meetingID := seedAssemblyMessage(t, st, "meeting_transcript", "", assemblyMeetingBody())
	beeperID := seedAssemblyMessage(t, st, "beeper", "", "chat text")
	blankID := seedAssemblyMessage(t, st, "discord", "", "   ")
	skippedID := seedAssemblyMessage(t, st, "calendar_event", "Skipped", "must not embed")

	snapshot, err := BeginSourceSnapshot(context.Background(), st)
	require.NoError(err)
	t.Cleanup(func() { _ = snapshot.Close() })
	assembler := CompositeAssembler{
		Policy: AssemblyPolicy{
			MaxChunkRunes: 12,
			SkipMessage: func(row AssemblyMessage) bool {
				return row.ID == skippedID
			},
		},
		Chat: chatAssemblerStub{},
	}

	scopes := []AffectedScope{
		{Kind: "email", MessageID: emailID},
		{Kind: "wrong-stale-kind", MessageID: beeperID},
		{Kind: "meeting_transcript", MessageID: meetingID},
		{Kind: "sms", MessageID: smsID},
		{Kind: "discord", MessageID: blankID},
		{Kind: "calendar_event", MessageID: skippedID},
		{Kind: "duplicate-other-kind", MessageID: emailID},
		{Kind: "duplicate-meeting", MessageID: meetingID},
	}
	docs, err := assembler.AssembleScopes(context.Background(), snapshot, scopes)
	require.NoError(err)

	assert.Equal([]string{
		"chat-specialized",
		fmt.Sprintf("meeting:%d", meetingID),
		fmt.Sprintf("message:%d", emailID),
		fmt.Sprintf("message:%d", smsID),
	}, documentKeys(docs))
	ordinary := documentByKey(t, docs, fmt.Sprintf("message:%d", emailID))
	require.Greater(len(ordinary.Chunks), 1)
	for i, chunk := range ordinary.Chunks {
		assert.Equal(i, chunk.ChunkIndex)
		assert.Equal(vector.SourceBasisSubjectBody, chunk.SourceBasis)
	}
	assert.Equal(0, ordinary.Chunks[0].SourceCharStart)
	assert.Equal(len([]rune("Subject: Roadmap\n\nfirst second third fourth fifth sixth")), ordinary.Chunks[len(ordinary.Chunks)-1].SourceCharEnd)

	sms := documentByKey(t, docs, fmt.Sprintf("message:%d", smsID))
	require.Len(sms.Chunks, 1)
	assert.Equal("ordinary SMS", sms.Chunks[0].Text)
}

func TestCompositeAssembler_BeeperUsesOrdinarySingletonUntilChatAssemblerIsInjected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	id := seedAssemblyMessage(t, st, "beeper", "", "fallback chat text")
	snapshot, err := BeginSourceSnapshot(context.Background(), st)
	require.NoError(err)
	t.Cleanup(func() { _ = snapshot.Close() })

	docs, err := (CompositeAssembler{Policy: AssemblyPolicy{MaxChunkRunes: 100}}).
		AssembleScopes(context.Background(), snapshot, []AffectedScope{{MessageID: id}})
	require.NoError(err)
	require.Len(docs, 1)
	assert.Equal(fmt.Sprintf("message:%d", id), docs[0].Key)
	assert.Equal(vector.SourceBasisSubjectBody, docs[0].Chunks[0].SourceBasis)
}

func TestCompositeAssembler_MessageScopeDedupIgnoresStalePayloadFields(t *testing.T) {
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	id := seedAssemblyMessage(t, st, "email", "One", "one body")
	snapshot, err := BeginSourceSnapshot(context.Background(), st)
	require.NoError(err)
	t.Cleanup(func() { _ = snapshot.Close() })

	docs, err := (CompositeAssembler{Policy: AssemblyPolicy{MaxChunkRunes: 100}}).AssembleScopes(
		context.Background(), snapshot, []AffectedScope{
			{
				Kind: "email", MessageID: id, ConversationID: 11,
				UTCStart: time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC),
				UTCEnd:   time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC),
			},
			{
				Kind: "stale-kind", MessageID: id, ConversationID: 99,
				UTCStart: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
				UTCEnd:   time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
			},
		})
	require.NoError(err)
	require.Len(docs, 1)
	assert.Equal(t, fmt.Sprintf("message:%d", id), docs[0].Key)
}

func TestCompositeAssembler_DistinctConversationKindsRemainSeparateForRouting(t *testing.T) {
	start := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	got := deduplicateScopes([]AffectedScope{
		{Kind: "meeting_transcript", ConversationID: 7, UTCStart: start, UTCEnd: end},
		{Kind: "beeper", ConversationID: 7, UTCStart: start, UTCEnd: end},
	})

	require.Len(t, got, 2)
	assert.Equal(t, []string{"beeper", "meeting_transcript"}, []string{got[0].Kind, got[1].Kind})
}

func TestSourceSnapshot_UndatedScopeExcludesDatedConversationRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("test", fmt.Sprintf("undated-source-%d", time.Now().UnixNano()))
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID,
		fmt.Sprintf("undated-conversation-%d", time.Now().UnixNano()), "Undated scope")
	require.NoError(err)
	insert := func(sourceID string, sentAt sql.NullTime) int64 {
		id, insertErr := st.UpsertMessage(&store.Message{
			ConversationID: conversationID, SourceID: source.ID,
			SourceMessageID: sourceID, MessageType: "beeper", SentAt: sentAt,
		})
		require.NoError(insertErr)
		require.NoError(st.UpsertMessageBody(id,
			sql.NullString{String: sourceID + " body", Valid: true}, sql.NullString{}))
		return id
	}
	undatedID := insert("undated", sql.NullTime{})
	insert("dated", sql.NullTime{Time: time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), Valid: true})

	snapshot, err := BeginSourceSnapshot(context.Background(), st)
	require.NoError(err)
	t.Cleanup(func() { _ = snapshot.Close() })
	rows, err := snapshot.Messages(context.Background(), AffectedScope{
		Kind: "beeper", ConversationID: conversationID, Undated: true,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(undatedID, rows[0].ID)

	deduplicated := deduplicateScopes([]AffectedScope{
		{Kind: "beeper", ConversationID: conversationID},
		{Kind: "beeper", ConversationID: conversationID, Undated: true},
	})
	assert.Len(deduplicated, 2, "undated and unbounded selectors have different identities")
}

func seedAssemblyMessage(t *testing.T, st *store.Store, messageType, subject, body string) int64 {
	t.Helper()
	source, err := st.GetOrCreateSource("test", fmt.Sprintf("source-%d", time.Now().UnixNano()))
	require.NoError(t, err)
	conversationID, err := st.EnsureConversation(source.ID, fmt.Sprintf("conversation-%d", time.Now().UnixNano()), "Synthetic conversation")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: fmt.Sprintf("message-%d", time.Now().UnixNano()),
		MessageType:     messageType,
		SentAt:          sql.NullTime{Time: time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), Valid: true},
		Subject:         sql.NullString{String: subject, Valid: subject != ""},
	})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBody(id,
		sql.NullString{String: body, Valid: true}, sql.NullString{}))
	return id
}

func documentKeys(docs []Document) []string {
	keys := make([]string, len(docs))
	for i := range docs {
		keys[i] = docs[i].Key
	}
	return keys
}

func documentByKey(t *testing.T, docs []Document, key string) Document {
	t.Helper()
	for _, doc := range docs {
		if doc.Key == key {
			return doc
		}
	}
	require.FailNow(t, "document not found", key)
	return Document{}
}

func assemblyMeetingBody() string {
	return "Weekly sync\nWhen: 2026-08-08 09:00\nAttendees: Alice, Bob\n\nDecision summary.\n\nTranscript:\n[00:01] Alice: First turn.\n[00:03] Bob: Second turn."
}
