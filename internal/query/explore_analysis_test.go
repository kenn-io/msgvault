package query

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExploreGroupsAggregatesCompleteLogicalPopulation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	first := b.AddSourceWithType("archive-a@example.com", "gmail")
	second := b.AddSourceWithType("archive-b@example.com", "imap")
	b.AddMessage(MessageOpt{SourceID: first, Subject: "One", SizeEstimate: 100})
	b.AddMessage(MessageOpt{SourceID: first, Subject: "Two", SizeEstimate: 200})
	b.AddMessage(MessageOpt{SourceID: second, Subject: "Three", SizeEstimate: 300})

	result, err := b.BuildEngine().ExploreGroups(context.Background(), ExploreGroupRequest{
		Explore: ExploreRequest{}, Dimension: "source",
		Sort: SortSpec{Field: "count", Direction: "desc"}, Page: PageSpec{Limit: 1},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 1)
	assertions.Equal(int64(2), result.TotalCount)
	assertions.Equal("1", result.Rows[0].Key)
	assertions.Equal(int64(2), result.Rows[0].Count)
	assertions.Equal(int64(300), result.Rows[0].EstimatedBytes)
	assertions.NotEmpty(result.CacheRevision)
}

func TestExploreGroupsParticipantLabelsUseDurableIdentityPrecedence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice Example")
	phone := b.AddPhoneParticipant("+15551234567", "")
	email := b.AddParticipant("email-only@example.net", "example.net", "")
	stableID := b.AddParticipant("", "", "")

	first := b.AddMessage(MessageOpt{SourceID: source, SenderID: &alice, Subject: "First"})
	b.AddFrom(first, alice, "Alice alias")
	b.AddTo(first, alice, "Alice duplicate membership")
	second := b.AddMessage(MessageOpt{SourceID: source, Subject: "Second"})
	b.AddTo(second, alice, "")
	third := b.AddMessage(MessageOpt{SourceID: source, Subject: "Phone"})
	b.AddTo(third, phone, "")
	fourth := b.AddMessage(MessageOpt{SourceID: source, Subject: "Email"})
	b.AddTo(fourth, email, "")
	fifth := b.AddMessage(MessageOpt{SourceID: source, Subject: "Stable ID"})
	b.AddTo(fifth, stableID, "")

	result, err := b.BuildEngine().ExploreGroups(context.Background(), ExploreGroupRequest{
		Explore: ExploreRequest{}, Dimension: "participant",
		Sort: SortSpec{Field: "key", Direction: "asc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 4)
	assertions.Equal([]ExploreGroupRow{
		{Key: "1", Label: "Alice Example", Count: 2, LatestAt: result.Rows[0].LatestAt},
		{Key: "2", Label: "+15551234567", Count: 1, LatestAt: result.Rows[1].LatestAt},
		{Key: "3", Label: "email-only@example.net", Count: 1, LatestAt: result.Rows[2].LatestAt},
		{Key: "4", Label: "Unknown person #4", Count: 1, LatestAt: result.Rows[3].LatestAt},
	}, result.Rows)
}

// linkedParticipantExploreFixture builds an archive where alice and her work
// alias are one linked identity cluster (canonical = alice, the smallest
// member ID): one entry lists BOTH aliases, one entry lists only the alias,
// and one entry involves only the unlinked bob.
func linkedParticipantExploreFixture(t *testing.T) (b *TestDataBuilder, alice, alias int64) {
	t.Helper()
	b = NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	alice = b.AddParticipant("alice@example.com", "example.com", "Alice Example")
	alias = b.AddParticipant("alice@work.example", "work.example", "Alice (Work)")
	bob := b.AddParticipant("bob@example.com", "example.com", "Bob Example")
	b.LinkCluster(alice, alias)

	both := b.AddMessage(MessageOpt{SourceID: source, Subject: "Both aliases", SizeEstimate: 100,
		SentAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)})
	b.AddTo(both, alice, "")
	b.AddCc(both, alias, "")
	aliasOnly := b.AddMessage(MessageOpt{SourceID: source, Subject: "Alias only", SizeEstimate: 30,
		SentAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)})
	b.AddTo(aliasOnly, alias, "")
	bobOnly := b.AddMessage(MessageOpt{SourceID: source, Subject: "Bob only", SizeEstimate: 7,
		SentAt: time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)})
	b.AddTo(bobOnly, bob, "")
	return b, alice, alias
}

func TestExploreGroupsMergesLinkedParticipantIdentities(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b, _, _ := linkedParticipantExploreFixture(t)

	result, err := b.BuildEngine().ExploreGroups(context.Background(), ExploreGroupRequest{
		Explore: ExploreRequest{}, Dimension: "participant",
		Sort: SortSpec{Field: "key", Direction: "asc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 2, "linked aliases must merge into one canonical row")
	assertions.Equal(int64(2), result.TotalCount)
	// The entry listing both aliases counts ONCE; the alias-only entry merges
	// into the canonical row; the label follows the cluster best-name policy
	// (smallest named member), not the latest alias's own name.
	assertions.Equal([]ExploreGroupRow{
		{Key: "1", Label: "Alice Example", Count: 2, EstimatedBytes: 130, LatestAt: result.Rows[0].LatestAt},
		{Key: "3", Label: "Bob Example", Count: 1, EstimatedBytes: 7, LatestAt: result.Rows[1].LatestAt},
	}, result.Rows)
}

func TestExploreParticipantFilterMatchesLinkedAliases(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b, alice, alias := linkedParticipantExploreFixture(t)
	engine := b.BuildEngine()

	byCanonical, err := engine.Explore(context.Background(), ExploreRequest{
		Context: Context{ParticipantIDs: []int64{alice}}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(byCanonical.Rows, 2, "canonical-ID filter must include alias-owned entries")
	assertions.Equal("source:1:message:msg2", byCanonical.Rows[0].Key, "alias-only entry")
	assertions.Equal("source:1:message:msg1", byCanonical.Rows[1].Key)

	byAlias, err := engine.Explore(context.Background(), ExploreRequest{
		Context: Context{ParticipantIDs: []int64{alias}}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	assertions.Len(byAlias.Rows, 2, "a member ID widens to its whole cluster")

	stats, err := engine.ExploreSelectionStats(context.Background(), ExploreSelectionRequest{
		Explore: ExploreRequest{Context: Context{ParticipantIDs: []int64{alice}}},
	})
	requirements.NoError(err)
	assertions.Equal(int64(2), stats.Count, "select-all preflight must match the merged group count")
}

func TestExploreSelectionStatsCoversPredicateAndExclusions(t *testing.T) {
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	first := b.AddMessage(MessageOpt{SourceID: source, Subject: "One", SizeEstimate: 100})
	b.AddAttachment(first, 10, "one.txt")
	b.AddMessage(MessageOpt{SourceID: source, Subject: "Two", SizeEstimate: 200})

	result, err := b.BuildEngine().ExploreSelectionStats(context.Background(), ExploreSelectionRequest{
		Explore: ExploreRequest{}, ExcludedKeys: []string{"source:1:message:msg1"},
	})
	require.NoError(t, err)
	assertions.Equal(int64(1), result.Count)
	assertions.Equal(int64(200), result.EstimatedBytes)
	assertions.NotEmpty(result.CacheRevision)
}

func TestExploreSelectionStatsCountsPerEntryActionSupport(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	gmail := b.AddSourceWithType("archive@example.com", "gmail")
	imap := b.AddSourceWithType("other@example.com", "imap")
	withFile := b.AddMessage(MessageOpt{SourceID: gmail, Subject: "Exportable"})
	b.AddAttachment(withFile, 10, "message.txt")
	b.AddMessage(MessageOpt{SourceID: gmail, Subject: "Open only"})
	b.AddMessage(MessageOpt{SourceID: imap, Subject: "Neither"})

	result, err := b.BuildEngine().ExploreSelectionStats(context.Background(), ExploreSelectionRequest{
		Explore: ExploreRequest{},
	})
	requirements.NoError(err)
	assertions.Equal(int64(3), result.Count)
	assertions.Equal(int64(1), result.ExportableCount)
	assertions.Equal(int64(2), result.OpenableCount)
}

func TestExploreSelectionStatsCanResolveExactDeletableMessageIDs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	first := b.AddMessage(MessageOpt{SourceID: source, Subject: "One"})
	second := b.AddMessage(MessageOpt{SourceID: source, Subject: "Two"})

	result, err := b.BuildEngine().ExploreSelectionStats(context.Background(), ExploreSelectionRequest{
		Explore: ExploreRequest{}, ExcludedKeys: []string{"source:1:message:msg1"},
		IncludeDeletableMessageIDs: true,
	})
	requirements.NoError(err)
	assertions.Equal(int64(1), result.Count)
	assertions.Equal([]int64{second}, result.DeletableMessageIDs)
	assertions.NotContains(result.DeletableMessageIDs, first)
}

func TestExploreSelectionStatsCanResolveExactRawExportMessageID(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	first := b.AddMessage(MessageOpt{SourceID: source, Subject: "One"})
	b.AddMessage(MessageOpt{SourceID: source, Subject: "Two"})

	result, err := b.BuildEngine().ExploreSelectionStats(context.Background(), ExploreSelectionRequest{
		Explore: ExploreRequest{}, IncludedKeys: []string{"source:1:message:msg1"},
	})
	requirements.NoError(err)
	requirements.NotNil(result.RawExportMessageID)
	assertions.Equal(first, *result.RawExportMessageID)

	bulk, err := b.BuildEngine().ExploreSelectionStats(context.Background(), ExploreSelectionRequest{
		Explore: ExploreRequest{},
	})
	requirements.NoError(err)
	assertions.Equal(int64(2), bulk.Count)
	assertions.Nil(bulk.RawExportMessageID, "bulk selections must not materialize raw message IDs")
}

func TestExploreFilesReturnsBoundedChronologicalAttachmentFacts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	older := b.AddMessage(MessageOpt{SourceID: source, Subject: "Older", SentAt: time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)})
	b.AddAttachment(older, 10, "older.txt")
	newer := b.AddMessage(MessageOpt{SourceID: source, Subject: "Newer", SentAt: time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC)})
	b.AddAttachmentWithMIME(77, newer, 20, "newer.pdf", "application/pdf")

	result, err := b.BuildEngine().ExploreFiles(context.Background(), ExploreFilesRequest{
		Explore: ExploreRequest{}, Page: PageSpec{Limit: 1},
	})
	requirements.NoError(err)
	requirements.Len(result.Files, 1)
	assertions.Equal(int64(2), result.TotalCount)
	assertions.Equal("newer.pdf", result.Files[0].Filename)
	assertions.Equal(int64(20), result.Files[0].Size)
	assertions.Equal(int64(77), result.Files[0].ID)
	assertions.Equal(newer, result.Files[0].MessageID)
	assertions.NotZero(result.Files[0].ConversationID)
	assertions.Equal("application/pdf", result.Files[0].MimeType)
	assertions.NotEmpty(result.CacheRevision)
}

func TestExploreFilesUsesDurableAttachmentIdentityForDuplicateMetadataPages(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	message := b.AddMessage(MessageOpt{SourceID: source, Subject: "Duplicates"})
	b.AddAttachmentWithID(41, message, 10, "same.txt")
	b.AddAttachmentWithID(42, message, 10, "same.txt")
	engine := b.BuildEngine()

	first, err := engine.ExploreFiles(context.Background(), ExploreFilesRequest{
		Explore: ExploreRequest{}, Page: PageSpec{Limit: 1},
	})
	requirements.NoError(err)
	requirements.Len(first.Files, 1)
	second, err := engine.ExploreFiles(context.Background(), ExploreFilesRequest{
		Explore: ExploreRequest{}, Page: PageSpec{Limit: 1, Offset: 1},
	})
	requirements.NoError(err)
	requirements.Len(second.Files, 1)

	assertions.Equal("source:1:message:msg1:file:41", first.Files[0].Key)
	assertions.Equal("source:1:message:msg1:file:42", second.Files[0].Key)
}

func TestExploreMatchCountsBatchesExactLogicalRowCounts(t *testing.T) {
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("+15550000000", messageTypeIMessage)
	const conversationID = int64(700)
	first := b.AddMessage(MessageOpt{SourceID: source, ConversationID: conversationID, MessageType: messageTypeIMessage, ConversationType: "direct_chat"})
	second := b.AddMessage(MessageOpt{SourceID: source, ConversationID: conversationID, MessageType: messageTypeIMessage, ConversationType: "direct_chat"})
	b.AddMessage(MessageOpt{SourceID: source, ConversationID: conversationID, MessageType: messageTypeIMessage, ConversationType: "direct_chat"})

	result, err := b.BuildEngine().ExploreMatchCounts(context.Background(), ExploreMatchCountsRequest{
		Explore: ExploreRequest{Search: SearchSpec{
			Mode: SearchFullText, Query: "alpha", CandidateMessageIDs: []int64{first, second}, LexicalIndexRevision: "fts5:test",
		}},
		RowKeys: []string{"source:1:conversation:700"},
	})
	require.NoError(t, err)
	assertions.Equal(map[string]int64{"source:1:conversation:700": 2}, result.Counts)
	assertions.NotEmpty(result.CacheRevision)
}
