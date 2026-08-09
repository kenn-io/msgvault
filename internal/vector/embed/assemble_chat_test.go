package embed

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
)

func TestChatWindowAssembler_SplitsGapDayAndBudget(t *testing.T) {
	f := newChatAssemblyFixture(t, AssemblyPolicy{
		ChatGap:              30 * time.Minute,
		MaxChunkRunes:        100,
		MaxDocumentUTF8Bytes: 4096,
	})
	f.seed("a", "2026-08-08T23:40:00Z", "Alice", "one")
	f.seed("b", "2026-08-08T23:55:00Z", "Bob", "two")
	f.seed("c", "2026-08-09T00:01:00Z", "Alice", "three")
	f.seed("d", "2026-08-09T00:40:00Z", "Bob", "four")

	docs := f.assemble(f.dayScope("2026-08-08"), f.dayScope("2026-08-09"))
	assert.Equal(t, []int{2, 1, 1}, documentMemberCounts(docs))

	budget := newChatAssemblyFixture(t, AssemblyPolicy{
		ChatGap:              30 * time.Minute,
		MaxChunkRunes:        100,
		MaxDocumentUTF8Bytes: 180,
	})
	budget.seed("a", "2026-08-08T09:00:00Z", "Alice", "one")
	budget.seed("b", "2026-08-08T09:01:00Z", "Bob", "two")
	assert.Equal(t, []int{1, 1}, documentMemberCounts(budget.assemble(budget.dayScope("2026-08-08"))))
}

func TestChatWindowAssembler_DeduplicatesSeedsAndScopeOrder(t *testing.T) {
	f := newChatAssemblyFixture(t, AssemblyPolicy{ChatGap: 30 * time.Minute, MaxChunkRunes: 100})
	first := f.seed("a", "2026-08-08T09:00:00Z", "Alice", "one")
	second := f.seed("b", "2026-08-08T09:05:00Z", "Bob", "two")
	scope := f.dayScope("2026-08-08")
	seeds := []AffectedScope{
		withSeed(scope, second),
		withSeed(scope, first),
		withSeed(scope, second),
	}

	want := f.assemble(seeds...)
	reversed := slices.Clone(seeds)
	slices.Reverse(reversed)
	got := f.assemble(reversed...)

	require.Len(t, want, 1)
	assert.Equal(t, want, got)

	randomized := slices.Clone(seeds)
	rand.New(rand.NewSource(11)).Shuffle(len(randomized), func(i, j int) { //nolint:gosec // Deterministic fixture order.
		randomized[i], randomized[j] = randomized[j], randomized[i]
	})
	assert.Equal(t, want, f.assemble(randomized...))
}

func TestChatWindowAssembler_LateBackfillChangesOnlyAffectedKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newChatAssemblyFixture(t, AssemblyPolicy{ChatGap: 30 * time.Minute, MaxChunkRunes: 100})
	f.seed("first-anchor", "2026-08-08T09:00:00Z", "Alice", "one")
	f.seed("first-tail", "2026-08-08T09:05:00Z", "Bob", "two")
	f.seed("later-anchor", "2026-08-08T10:00:00Z", "Alice", "three")
	f.seed("later-tail", "2026-08-08T10:05:00Z", "Bob", "four")

	before := f.assemble(f.dayScope("2026-08-08"))
	require.Len(before, 2)
	f.seed("backfill", "2026-08-08T08:59:00Z", "Alice", "late")
	after := f.assemble(f.dayScope("2026-08-08"))
	require.Len(after, 2)

	assert.NotContains(documentKeys(after), before[0].Key)
	later := documentByKey(t, after, before[1].Key)
	assert.Equal(before[1].Revision, later.Revision)
	assert.Equal(before[1].Versions, later.Versions)
	assert.Equal(before[1].Chunks, later.Chunks,
		"a backfill in an earlier window must not renumber or revise a later window")
}

func TestChatWindowAssembler_NullAndEqualTimesUseMessageIDTieBreak(t *testing.T) {
	f := newChatAssemblyFixture(t, AssemblyPolicy{ChatGap: 30 * time.Minute, MaxChunkRunes: 100})
	nullFirst := f.seed("null-first", "", "Alice", "one")
	nullSecond := f.seed("null-second", "", "Bob", "two")
	equalFirst := f.seed("equal-first", "2026-08-08T09:00:00Z", "Alice", "three")
	equalSecond := f.seed("equal-second", "2026-08-08T09:00:00Z", "Bob", "four")

	docs := f.assemble(
		chatDayContextScope(f.conversationID, time.Time{}).selector,
		chatDayContextScope(f.conversationID, time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)).selector,
	)
	require.Len(t, docs, 2)
	assert.Equal(t, []int64{nullFirst, nullSecond}, documentMemberIDs(docs[0]))
	assert.Equal(t, []int64{equalFirst, equalSecond}, documentMemberIDs(docs[1]))
}

func TestChatWindowAssembler_RendersCanonicalContextAndExactBodySpans(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newChatAssemblyFixture(t, AssemblyPolicy{ChatGap: 30 * time.Minute, MaxChunkRunes: 5})
	shortID := f.seed("short", "2026-08-08T09:00:00Z", "Alice", "hello")
	longBody := "猫犬鳥魚馬牛羊猿"
	longID := f.seed("long", "2026-08-08T09:01:00Z", "", longBody)

	doc := onlyChatDocument(t, f.assemble(f.dayScope("2026-08-08")))
	assert.Equal("beeper-window", doc.Kind)
	assert.Equal(fmt.Sprintf("chat:%d:2026-08-08", f.conversationID), doc.ScopeKey)
	assert.Equal(f.conversationID, doc.MetadataVersion.ConversationID)
	assert.NotEmpty(doc.MetadataVersion.Digest)
	require.NotEmpty(doc.Chunks)
	assert.Contains(doc.Chunks[0].Text, "Conversation: Synthetic chat")
	assert.Contains(doc.Chunks[0].Text, "Participants: Alice, Bob")
	assert.Contains(doc.Chunks[0].Text, "Date: 2026-08-08")
	assert.Contains(doc.Chunks[0].Text, "Alice (09:00): hello")

	byMessage := chunksByMessage(doc.Chunks)
	require.Len(byMessage[shortID], 1, "a short member must not be split because another member is long")
	require.Greater(len(byMessage[longID]), 1)
	assert.Contains(byMessage[longID][0].Text, "Unknown sender (09:01):")
	assertExactChatSpans(t, "hello", byMessage[shortID])
	assertExactChatSpans(t, longBody, byMessage[longID])
	for _, chunks := range byMessage {
		for i, chunk := range chunks {
			assert.Equal(i, chunk.ChunkIndex)
			assert.Equal(vector.SourceBasisBody, chunk.SourceBasis)
		}
	}
}

func TestChatWindowAssembler_UsesUTF8BytesForDocumentBudget(t *testing.T) {
	policy := AssemblyPolicy{
		ChatGap:              30 * time.Minute,
		MaxChunkRunes:        100,
		MaxDocumentUTF8Bytes: 334,
	}
	ascii := newChatAssemblyFixture(t, policy)
	ascii.seed("first", "2026-08-08T09:00:00Z", "Alice", strings.Repeat("a", 10))
	ascii.seed("second", "2026-08-08T09:01:00Z", "Bob", strings.Repeat("b", 10))
	assert.Equal(t, []int{2}, documentMemberCounts(ascii.assemble(ascii.dayScope("2026-08-08"))))

	unicode := newChatAssemblyFixture(t, policy)
	unicode.seed("first", "2026-08-08T09:00:00Z", "Alice", strings.Repeat("a", 10))
	unicode.seed("second", "2026-08-08T09:01:00Z", "Bob", strings.Repeat("猫", 10))
	assert.Equal(t, []int{1, 1}, documentMemberCounts(unicode.assemble(unicode.dayScope("2026-08-08"))),
		"equal rune counts with different UTF-8 sizes must use the byte budget")
}

func TestChatWindowAssembler_AccountsForPromptReserveAndCapsOneLargeMember(t *testing.T) {
	policy := AssemblyPolicy{
		ChatGap: 30 * time.Minute, MaxChunkRunes: 128, MaxDocumentUTF8Bytes: 260,
	}
	f := newChatAssemblyFixture(t, policy)
	f.seed("large", "2026-08-08T09:00:00Z", "Alice", strings.Repeat("🙂", 128))
	f.seed("next", "2026-08-08T09:01:00Z", "Bob", "next")
	docs := f.assemble(f.dayScope("2026-08-08"))
	require.NotEmpty(t, docs)

	for _, doc := range docs {
		input := DocumentInput{Chunks: make([]string, len(doc.Chunks))}
		for i, chunk := range doc.Chunks {
			input.Chunks[i] = chunk.Text
		}
		_, err := PackDocuments([]DocumentInput{input}, RequestLimits{
			MaxDocuments: 1, MaxChunks: 16_000, MaxUTF8Bytes: policy.MaxDocumentUTF8Bytes,
		})
		require.NoError(t, err)
	}
	assert.True(t, docs[0].Chunks[len(docs[0].Chunks)-1].Truncated)
}

func TestChatWindowAssembler_UsesStableBoundedMessageScopes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newChatAssemblyFixture(t, AssemblyPolicy{
		ChatGap: 24 * time.Hour, MaxChunkRunes: 100,
	})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, chatScopeMaxMessages+1)
	for i := range chatScopeMaxMessages + 1 {
		ids = append(ids, f.seed(fmt.Sprintf("bounded-%d", i),
			day.Add(time.Duration(i)*time.Minute).Format(time.RFC3339), "Alice", fmt.Sprintf("message %d", i)))
	}
	first := chatMessageContextScope(f.conversationID, day, ids[0])
	last := chatMessageContextScope(f.conversationID, day, ids[len(ids)-1])
	assert.NotEqual(first.key, last.key)

	docs := f.assemble(first.selector, last.selector)
	require.Len(docs, 2)
	assert.Equal(chatScopeMaxMessages+1, len(documentMemberIDs(docs[0]))+len(documentMemberIDs(docs[1])))
	for _, doc := range docs {
		assert.LessOrEqual(len(documentMemberIDs(doc)), chatScopeMaxMessages)
	}
}

func TestSourceSnapshot_ChatMessagesBoundsBodyBeforeAssembly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newChatAssemblyFixture(t, AssemblyPolicy{ChatGap: 30 * time.Minute, MaxChunkRunes: 100})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	id := f.seed("large-body", day.Format(time.RFC3339), "Alice",
		strings.Repeat("x", chatMessageBodyMaxChars+1))
	scope := chatMessageContextScope(f.conversationID, day, id)
	snapshot, err := BeginSourceSnapshot(t.Context(), f.store)
	require.NoError(err)
	rows, err := snapshot.ChatMessages(t.Context(), scope.selector)
	require.NoError(err)
	require.NoError(snapshot.Close())
	require.Len(rows, 1)
	assert.Equal(chatMessageBodyMaxChars, utf8.RuneCountInString(rows[0].Body))
	assert.True(rows[0].BodyTruncated)
}

func TestChatWindowAssembler_OldScopesDoNotResolveChangedMessageLive(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		f := newChatAssemblyFixture(t, AssemblyPolicy{ChatGap: 30 * time.Minute, MaxChunkRunes: 100})
		kept := f.seed("kept", "2026-08-08T09:00:00Z", "Alice", "kept")
		changed := f.seed("deleted", "2026-08-08T09:05:00Z", "Bob", "deleted")
		require.NoError(t, f.exec("UPDATE messages SET deleted_at = ? WHERE id = ?", "2026-08-09 00:00:00", changed))

		doc := onlyChatDocument(t, f.assemble(withSeed(f.dayScope("2026-08-08"), changed)))
		assert.Equal(t, []int64{kept}, documentMemberIDs(doc))
	})

	t.Run("conversation move", func(t *testing.T) {
		f := newChatAssemblyFixture(t, AssemblyPolicy{ChatGap: 30 * time.Minute, MaxChunkRunes: 100})
		kept := f.seed("kept", "2026-08-08T09:00:00Z", "Alice", "kept")
		changed := f.seed("moved", "2026-08-08T09:05:00Z", "Bob", "moved")
		newConversation := f.addConversation("Moved chat")
		require.NoError(t, f.exec("UPDATE messages SET conversation_id = ? WHERE id = ?", newConversation, changed))

		doc := onlyChatDocument(t, f.assemble(withSeed(f.dayScope("2026-08-08"), changed)))
		assert.Equal(t, []int64{kept}, documentMemberIDs(doc))
	})

	t.Run("timestamp day move", func(t *testing.T) {
		f := newChatAssemblyFixture(t, AssemblyPolicy{ChatGap: 30 * time.Minute, MaxChunkRunes: 100})
		kept := f.seed("kept", "2026-08-08T09:00:00Z", "Alice", "kept")
		changed := f.seed("moved", "2026-08-08T09:05:00Z", "Bob", "moved")
		require.NoError(t, f.exec("UPDATE messages SET sent_at = ? WHERE id = ?", "2026-08-09 09:05:00", changed))

		doc := onlyChatDocument(t, f.assemble(withSeed(f.dayScope("2026-08-08"), changed)))
		assert.Equal(t, []int64{kept}, documentMemberIDs(doc))
	})
}

func TestChatWindowAssembler_RevisionTracksSourceAndMetadataSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newChatAssemblyFixture(t, AssemblyPolicy{ChatGap: 30 * time.Minute, MaxChunkRunes: 100})
	messageID := f.seed("message", "2026-08-08T09:00:00Z", "Alice", "before")
	scope := f.dayScope("2026-08-08")
	baseline := onlyChatDocument(t, f.assemble(scope))

	require.NoError(f.store.UpsertMessageBody(messageID,
		sql.NullString{String: "after", Valid: true}, sql.NullString{}))
	messageEdit := onlyChatDocument(t, f.assemble(scope))
	assert.NotEqual(baseline.Revision, messageEdit.Revision)
	assert.Equal(baseline.MetadataVersion.Digest, messageEdit.MetadataVersion.Digest)

	require.NoError(f.exec("UPDATE conversations SET title = ? WHERE id = ?", "Renamed chat", f.conversationID))
	titleEdit := onlyChatDocument(t, f.assemble(scope))
	assert.NotEqual(messageEdit.MetadataVersion.Digest, titleEdit.MetadataVersion.Digest)
	assert.NotEqual(messageEdit.Revision, titleEdit.Revision)

	require.NoError(f.exec("UPDATE conversation_participants SET role = ? WHERE conversation_id = ? AND participant_id = ?",
		"admin", f.conversationID, f.aliceID))
	membershipEdit := onlyChatDocument(t, f.assemble(scope))
	assert.NotEqual(titleEdit.MetadataVersion.Digest, membershipEdit.MetadataVersion.Digest)
	assert.NotEqual(titleEdit.Revision, membershipEdit.Revision)

	require.NoError(f.exec("UPDATE participants SET updated_at = ? WHERE id = ?", "2031-01-02 03:04:05", f.aliceID))
	participantEdit := onlyChatDocument(t, f.assemble(scope))
	assert.NotEqual(membershipEdit.MetadataVersion.Digest, participantEdit.MetadataVersion.Digest)
	assert.NotEqual(membershipEdit.Revision, participantEdit.Revision)
}

type chatAssemblyFixture struct {
	t              *testing.T
	store          *store.Store
	sourceID       int64
	conversationID int64
	aliceID        int64
	bobID          int64
	assembler      ChatWindowAssembler
}

func newChatAssemblyFixture(t *testing.T, policy AssemblyPolicy) *chatAssemblyFixture {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("test", fmt.Sprintf("chat-source-%d", time.Now().UnixNano()))
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "chat", "group_chat", "Synthetic chat")
	require.NoError(t, err)
	aliceID, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(t, err)
	bobID, err := st.EnsureParticipant("bob@example.test", "Bob", "example.test")
	require.NoError(t, err)
	require.NoError(t, st.EnsureConversationParticipant(conversationID, bobID, "member"))
	require.NoError(t, st.EnsureConversationParticipant(conversationID, aliceID, "member"))
	return &chatAssemblyFixture{
		t: t, store: st, sourceID: source.ID, conversationID: conversationID,
		aliceID: aliceID, bobID: bobID, assembler: ChatWindowAssembler{Policy: policy},
	}
}

func (f *chatAssemblyFixture) seed(sourceID, timestamp, sender, body string) int64 {
	f.t.Helper()
	var sentAt sql.NullTime
	if timestamp != "" {
		parsed, err := time.Parse(time.RFC3339, timestamp)
		require.NoError(f.t, err)
		sentAt = sql.NullTime{Time: parsed, Valid: true}
	}
	var senderID sql.NullInt64
	switch sender {
	case "Alice":
		senderID = sql.NullInt64{Int64: f.aliceID, Valid: true}
	case "Bob":
		senderID = sql.NullInt64{Int64: f.bobID, Valid: true}
	}
	id, err := f.store.UpsertMessage(&store.Message{
		ConversationID: f.conversationID, SourceID: f.sourceID,
		SourceMessageID: sourceID, MessageType: "beeper", SentAt: sentAt, SenderID: senderID,
	})
	require.NoError(f.t, err)
	require.NoError(f.t, f.store.UpsertMessageBody(id,
		sql.NullString{String: body, Valid: true}, sql.NullString{}))
	return id
}

func (f *chatAssemblyFixture) addConversation(title string) int64 {
	f.t.Helper()
	id, err := f.store.EnsureConversationWithType(f.sourceID,
		fmt.Sprintf("chat-%d", time.Now().UnixNano()), "group_chat", title)
	require.NoError(f.t, err)
	return id
}

func (f *chatAssemblyFixture) dayScope(date string) AffectedScope {
	f.t.Helper()
	start, err := time.Parse("2006-01-02", date)
	require.NoError(f.t, err)
	return chatDayContextScope(f.conversationID, start).selector
}

func (f *chatAssemblyFixture) assemble(scopes ...AffectedScope) []Document {
	f.t.Helper()
	snapshot, err := BeginSourceSnapshot(context.Background(), f.store)
	require.NoError(f.t, err)
	docs, assembleErr := f.assembler.AssembleScopes(context.Background(), snapshot, scopes)
	require.NoError(f.t, snapshot.Close())
	require.NoError(f.t, assembleErr)
	return docs
}

func (f *chatAssemblyFixture) exec(query string, args ...any) error {
	_, err := f.store.DB().Exec(f.store.Rebind(query), args...)
	return err
}

func withSeed(scope AffectedScope, messageID int64) AffectedScope {
	scope.MessageID = messageID
	return scope
}

func onlyChatDocument(t *testing.T, docs []Document) Document {
	t.Helper()
	require.Len(t, docs, 1)
	return docs[0]
}

func documentMemberCounts(docs []Document) []int {
	counts := make([]int, len(docs))
	for i, doc := range docs {
		counts[i] = len(documentMemberIDs(doc))
	}
	return counts
}

func documentMemberIDs(doc Document) []int64 {
	ids := make([]int64, 0)
	for _, version := range doc.Versions {
		ids = append(ids, version.MessageID)
	}
	return ids
}

func chunksByMessage(chunks []OwnedChunk) map[int64][]OwnedChunk {
	result := make(map[int64][]OwnedChunk)
	for _, chunk := range chunks {
		result[chunk.MessageID] = append(result[chunk.MessageID], chunk)
	}
	return result
}

func assertExactChatSpans(t *testing.T, body string, chunks []OwnedChunk) {
	t.Helper()
	runes := []rune(body)
	lastEnd := 0
	var reconstructed strings.Builder
	for _, chunk := range chunks {
		require.LessOrEqual(t, chunk.SourceCharStart, chunk.SourceCharEnd)
		assert.Equal(t, lastEnd, chunk.SourceCharStart)
		require.LessOrEqual(t, chunk.SourceCharEnd, len(runes))
		reconstructed.WriteString(string(runes[chunk.SourceCharStart:chunk.SourceCharEnd]))
		assert.LessOrEqual(t, utf8.RuneCountInString(string(runes[chunk.SourceCharStart:chunk.SourceCharEnd])), 5)
		lastEnd = chunk.SourceCharEnd
	}
	assert.Equal(t, body, reconstructed.String())
}
