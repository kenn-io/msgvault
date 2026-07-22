package query

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildFastPathEquivalenceEngine assembles an archive exercising every shape
// the two-phase listing fast paths must reproduce: emails with recipients and
// per-message display names, a participant-less message, calendar/meeting
// entries, source-deleted rows, direct and group chat conversations with
// senders, conversation participants and titles, phone-only and empty-label
// participants, owner participants with a clustered alias (counterpart
// resolution), and attachments across MIME families on both email and chat
// messages.
func buildFastPathEquivalenceEngine(t *testing.T) (*DuckDBEngine, fastPathFixtureIDs) {
	t.Helper()
	b := NewTestDataBuilder(t)
	gmail := b.AddSource("owner@example.com")
	chats := b.AddSourceWithType("+15550001111", "imessage")

	alice := b.AddParticipant("alice@example.com", "example.com", "Alice Adams")
	bob := b.AddParticipant("bob@corp.example", "corp.example", "")
	carol := b.AddPhoneParticipant("+15550002222", "Carol")
	owner := b.AddParticipant("owner@example.com", "example.com", "Owner")
	ownerAlias := b.AddPhoneParticipant("+15550001111", "")
	b.AddOwnerParticipant(gmail, owner)
	b.LinkCluster(owner, ownerAlias)

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	email1 := b.AddMessage(MessageOpt{Subject: "Quarterly report", SentAt: base, SizeEstimate: 100, SourceID: gmail})
	b.AddFrom(email1, alice, "Alice Adams")
	b.AddTo(email1, owner, "")
	b.AddAttachmentWithMIME(1, email1, 2048, "report.pdf", "application/pdf")
	b.AddAttachmentWithMIME(2, email1, 512, "notes.txt", "text/plain")

	email2 := b.AddMessage(MessageOpt{Snippet: "hello there", SentAt: base.Add(time.Hour), SourceID: gmail})
	b.AddFrom(email2, bob, "")
	b.AddTo(email2, owner, "")
	b.AddCc(email2, alice, "")

	deletedAt := base.Add(30 * time.Minute)
	emailDeleted := b.AddMessage(MessageOpt{
		Subject: "Old thread", SentAt: base.Add(2 * time.Hour), SourceID: gmail, DeletedFromSourceAt: &deletedAt,
	})
	b.AddFrom(emailDeleted, alice, "")
	b.AddAttachmentWithMIME(3, emailDeleted, 9000, "archive.zip", "application/zip")

	b.AddMessage(MessageOpt{Subject: "Orphan draft", SentAt: base.Add(3 * time.Hour), SourceID: gmail})

	event := b.AddMessage(MessageOpt{
		Subject: "Standup", MessageType: messageTypeCalendar, SentAt: base.Add(4 * time.Hour), SourceID: gmail,
	})
	b.AddFrom(event, alice, "")
	b.AddMessage(MessageOpt{
		Subject: "Weekly sync", MessageType: "meeting_transcript", SentAt: base.Add(5 * time.Hour), SourceID: gmail,
	})

	const directConv = int64(900)
	chat1 := b.AddMessage(MessageOpt{
		Snippet: "yo", MessageType: messageTypeIMessage, ConversationType: "direct_chat",
		ConversationID: directConv, SourceID: chats, SenderID: &carol, SentAt: base.Add(10 * time.Minute),
	})
	chat2 := b.AddMessage(MessageOpt{
		Snippet: "hey!", MessageType: messageTypeIMessage, ConversationType: "direct_chat",
		ConversationID: directConv, SourceID: chats, SenderID: &ownerAlias, IsFromMe: true,
		SentAt: base.Add(20 * time.Minute),
	})
	b.AddAttachmentWithMIME(4, chat2, 4096, "photo.jpg", "image/jpeg")
	b.AddConversationParticipant(directConv, carol)
	b.AddConversationParticipant(directConv, ownerAlias)

	const groupConv = int64(901)
	group1 := b.AddMessage(MessageOpt{
		Snippet: "dinner?", MessageType: messageTypeIMessage, ConversationType: "group_chat",
		ConversationID: groupConv, ConversationTitle: "Family", SourceID: chats,
		SenderID: &carol, SentAt: base.Add(6 * time.Hour),
	})
	b.AddMessage(MessageOpt{
		Snippet: "sure", MessageType: messageTypeIMessage, ConversationType: "group_chat",
		ConversationID: groupConv, SourceID: chats, SenderID: &ownerAlias, IsFromMe: true,
		SentAt: base.Add(6*time.Hour + time.Minute),
	})
	b.AddConversationParticipant(groupConv, carol)
	b.AddConversationParticipant(groupConv, ownerAlias)
	b.AddConversationParticipant(groupConv, alice)

	engine := b.BuildEngine()
	return engine, fastPathFixtureIDs{
		gmailSource: gmail, chatSource: chats, alice: alice,
		email1: email1, email2: email2, chat1: chat1, chat2: chat2, group1: group1,
		base: base,
	}
}

type fastPathFixtureIDs struct {
	gmailSource int64
	chatSource  int64
	alice       int64
	email1      int64
	email2      int64
	chat1       int64
	chat2       int64
	group1      int64
	base        time.Time
}

// runExploreBothPaths executes the same request through the fast two-phase
// listing query and the legacy single-pass query on the same engine.
func runExploreBothPaths(t *testing.T, engine *DuckDBEngine, request ExploreRequest) (fast, legacy *ExploreResponse) {
	t.Helper()
	engine.exploreFastPathDisabled = false
	fast, err := engine.Explore(context.Background(), request)
	require.NoError(t, err)
	engine.exploreFastPathDisabled = true
	legacy, err = engine.Explore(context.Background(), request)
	require.NoError(t, err)
	engine.exploreFastPathDisabled = false
	return fast, legacy
}

func TestExploreListingFastPathMatchesLegacy(t *testing.T) {
	engine, ids := buildFastPathEquivalenceEngine(t)
	generation := int64(4)
	after := ids.base.Add(30 * time.Minute)
	before := ids.base.Add(5 * time.Hour)

	tests := []struct {
		name    string
		request ExploreRequest
	}{
		{name: "full listing", request: ExploreRequest{Page: PageSpec{Limit: 50}}},
		{name: "small page", request: ExploreRequest{Page: PageSpec{Limit: 3}}},
		{name: "offset page", request: ExploreRequest{Page: PageSpec{Limit: 3, Offset: 2}}},
		{name: "offset beyond end", request: ExploreRequest{Page: PageSpec{Limit: 3, Offset: 500}}},
		{name: "source filter", request: ExploreRequest{
			Context: Context{SourceIDs: []int64{ids.chatSource}}, Page: PageSpec{Limit: 50},
		}},
		{name: "message type filter", request: ExploreRequest{
			Context: Context{MessageTypes: []string{"email"}}, Page: PageSpec{Limit: 50},
		}},
		{name: "time range filter", request: ExploreRequest{
			Context: Context{After: &after, Before: &before}, Page: PageSpec{Limit: 50},
		}},
		{name: "deletion active", request: ExploreRequest{
			Context: Context{Deletion: DeletionActive}, Page: PageSpec{Limit: 50},
		}},
		{name: "deletion deleted", request: ExploreRequest{
			Context: Context{Deletion: DeletionDeleted}, Page: PageSpec{Limit: 50},
		}},
		{name: "no matches", request: ExploreRequest{
			Context: Context{MessageTypes: []string{"telegram"}}, Page: PageSpec{Limit: 50},
		}},
		{name: "full text candidates", request: ExploreRequest{
			Search: SearchSpec{
				Mode: SearchFullText, Query: "report",
				CandidateMessageIDs:  []int64{ids.email1, ids.chat1, ids.chat2},
				LexicalIndexRevision: "fts5:test",
			},
			Page: PageSpec{Limit: 50},
		}},
		{name: "empty candidates", request: ExploreRequest{
			Search: SearchSpec{
				Mode: SearchFullText, Query: "nothing",
				CandidateMessageIDs: []int64{}, LexicalIndexRevision: "fts5:test",
			},
			Page: PageSpec{Limit: 50},
		}},
		{name: "semantic strongest match inside chat group", request: ExploreRequest{
			Search: SearchSpec{
				Mode: SearchSemantic, Query: "photos",
				CandidateMessageIDs: []int64{ids.chat2, ids.chat1, ids.group1},
				VectorGeneration:    &generation,
			},
			Page: PageSpec{Limit: 50},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fast, legacy := runExploreBothPaths(t, engine, tt.request)
			assert.Equal(t, legacy, fast)
			assert.NotNil(t, fast.Rows)
		})
	}
}

// TestExploreListingFastPathPagesTileFullListing pins that fast-path
// LIMIT/OFFSET pages concatenate into exactly the full listing, so
// offset-based cursors keep working across pages.
func TestExploreListingFastPathPagesTileFullListing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	engine, _ := buildFastPathEquivalenceEngine(t)
	full, err := engine.Explore(context.Background(), ExploreRequest{Page: PageSpec{Limit: 100}})
	require.NoError(err)
	require.NotEmpty(full.Rows)

	var tiled []EntryRow
	for offset := 0; ; offset += 3 {
		page, err := engine.Explore(context.Background(), ExploreRequest{Page: PageSpec{Limit: 3, Offset: offset}})
		require.NoError(err)
		if len(page.Rows) == 0 {
			break
		}
		assert.Equal(full.TotalCount, page.TotalCount)
		tiled = append(tiled, page.Rows...)
	}
	assert.Equal(full.Rows, tiled)
}

// runSearchFilesBothPaths executes the same request through the fast
// two-phase file page query and the legacy single-pass query.
func runSearchFilesBothPaths(t *testing.T, engine *DuckDBEngine, request FileSearchRequest) (fast, legacy *FileSearchResponse) {
	t.Helper()
	engine.exploreFastPathDisabled = false
	fast, err := engine.SearchFiles(context.Background(), request)
	require.NoError(t, err)
	engine.exploreFastPathDisabled = true
	legacy, err = engine.SearchFiles(context.Background(), request)
	require.NoError(t, err)
	engine.exploreFastPathDisabled = false
	return fast, legacy
}

func TestSearchFilesFastPathMatchesLegacy(t *testing.T) {
	engine, ids := buildFastPathEquivalenceEngine(t)
	before := ids.base.Add(90 * time.Minute)

	tests := []struct {
		name    string
		request FileSearchRequest
	}{
		{name: "full listing", request: FileSearchRequest{Page: PageSpec{Limit: 50}}},
		{name: "small page", request: FileSearchRequest{Page: PageSpec{Limit: 2}}},
		{name: "offset page", request: FileSearchRequest{Page: PageSpec{Limit: 2, Offset: 1}}},
		{name: "filename ascending", request: FileSearchRequest{
			Sort: SortSpec{Field: "filename", Direction: "asc"}, Page: PageSpec{Limit: 50},
		}},
		{name: "size descending", request: FileSearchRequest{
			Sort: SortSpec{Field: "size", Direction: "desc"}, Page: PageSpec{Limit: 50},
		}},
		{name: "occurred ascending", request: FileSearchRequest{
			Sort: SortSpec{Field: "occurred_at", Direction: "asc"}, Page: PageSpec{Limit: 50},
		}},
		{name: "filename query", request: FileSearchRequest{
			FilenameQuery: "RepOrt", Page: PageSpec{Limit: 50},
		}},
		{name: "mime family filter", request: FileSearchRequest{
			MIMEFamilies: []FileMIMEFamily{FileMIMEImage, FileMIMEPDF}, Page: PageSpec{Limit: 50},
		}},
		{name: "explore source filter", request: FileSearchRequest{
			Explore: ExploreRequest{Context: Context{SourceIDs: []int64{ids.chatSource}}},
			Page:    PageSpec{Limit: 50},
		}},
		{name: "explore time filter", request: FileSearchRequest{
			Explore: ExploreRequest{Context: Context{Before: &before}},
			Page:    PageSpec{Limit: 50},
		}},
		{name: "explore deletion filter", request: FileSearchRequest{
			Explore: ExploreRequest{Context: Context{Deletion: DeletionActive}},
			Page:    PageSpec{Limit: 50},
		}},
		{name: "no matches", request: FileSearchRequest{
			FilenameQuery: "does-not-exist", Page: PageSpec{Limit: 50},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fast, legacy := runSearchFilesBothPaths(t, engine, tt.request)
			assert.Equal(t, legacy, fast)
			assert.NotNil(t, fast.Files)
		})
	}
}

// TestExploreParticipantContextKeepsLegacyPath pins the fork rule: participant
// and domain context filters render list_contains predicates whose evaluation
// already assembles participant lists archive-wide, so the two-phase rescan
// would pay that twice.
func TestExploreParticipantContextKeepsLegacyPath(t *testing.T) {
	assert := assert.New(t)
	assert.False(exploreConditionsTouchParticipantLists(ExploreRequest{}))
	assert.False(exploreConditionsTouchParticipantLists(ExploreRequest{
		Context: Context{SourceIDs: []int64{1}, MessageTypes: []string{"email"}},
	}))
	assert.True(exploreConditionsTouchParticipantLists(ExploreRequest{
		Context: Context{ParticipantIDs: []int64{7}},
	}))
	assert.True(exploreConditionsTouchParticipantLists(ExploreRequest{
		Context: Context{Domains: []string{"example.com"}},
	}))
}

// TestExploreParticipantFilterListingStillCorrect exercises the legacy path
// the fork preserves for participant-scoped listings.
func TestExploreParticipantFilterListingStillCorrect(t *testing.T) {
	engine, ids := buildFastPathEquivalenceEngine(t)
	response, err := engine.Explore(context.Background(), ExploreRequest{
		Context: Context{ParticipantIDs: []int64{ids.alice}},
		Page:    PageSpec{Limit: 50},
	})
	require.NoError(t, err)
	require.NotEmpty(t, response.Rows)
	for _, row := range response.Rows {
		assert.Contains(t, row.ParticipantIDs, ids.alice, "row %s must include the filtered participant", row.Key)
	}
}
