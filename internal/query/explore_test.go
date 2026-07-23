package query

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExploreKeepsSemanticMatchStateConstantPerLogicalRow(t *testing.T) {
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("+15550000000", messageTypeIMessage)
	const conversationID = int64(700)
	first := b.AddMessage(MessageOpt{
		SourceID: sourceID, ConversationID: conversationID, MessageType: messageTypeIMessage,
		ConversationType: "direct_chat", SentAt: time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC),
	})
	second := b.AddMessage(MessageOpt{
		SourceID: sourceID, ConversationID: conversationID, MessageType: messageTypeIMessage,
		ConversationType: "direct_chat", SentAt: time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC),
	})
	b.AddMessage(MessageOpt{
		SourceID: sourceID, ConversationID: conversationID, MessageType: messageTypeIMessage,
		ConversationType: "direct_chat", SentAt: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
	})
	engine := b.BuildEngine()
	generation := int64(7)

	tests := []struct {
		name          string
		search        SearchSpec
		wantStrongest *int64
	}{
		{name: "no search"},
		{
			name:   "full text",
			search: SearchSpec{Mode: SearchFullText, Query: "alpha", CandidateMessageIDs: []int64{first, second}, LexicalIndexRevision: "fts5:test"},
		},
		{
			name:          "semantic strongest is not chronological anchor",
			search:        SearchSpec{Mode: SearchSemantic, Query: "alpha", CandidateMessageIDs: []int64{first, second}, VectorGeneration: &generation},
			wantStrongest: &first,
		},
		{
			name: "hybrid strongest is not chronological anchor",
			search: SearchSpec{
				Mode: SearchHybrid, Query: "alpha", CandidateMessageIDs: []int64{first, second},
				LexicalIndexRevision: "fts5:test", VectorGeneration: &generation,
			},
			wantStrongest: &first,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := engine.Explore(context.Background(), ExploreRequest{Search: tt.search})
			requirements.NoError(err)
			requirements.Len(result.Rows, 1)
			assertExploreBoundedStrongestMatch(t, result.Rows[0], tt.wantStrongest)
		})
	}
}

func TestExploreTenThousandFragmentConversationKeepsConstantMatchState(t *testing.T) {
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("+15550000000", messageTypeIMessage)
	const conversationID = int64(700)
	for range 10_000 {
		b.AddMessage(MessageOpt{
			SourceID: sourceID, ConversationID: conversationID, MessageType: messageTypeIMessage,
			ConversationType: "direct_chat",
		})
	}

	result, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{})
	requirements.NoError(err)
	requirements.Len(result.Rows, 1)
	requirements.Equal(int64(10_000), result.Rows[0].MessageCount)
	assertExploreBoundedStrongestMatch(t, result.Rows[0], nil)
}

// TestExploreMessageTypeEmailIncludesLegacyRows pins the legacy-row rule for
// the explore context filter: rows imported before message_type existed carry
// an empty value and count as email (see emailOnlyFilterMsg,
// store.IsEmailMessageType), so an "email" filter must include them while
// non-email filters must not.
func TestExploreMessageTypeEmailIncludesLegacyRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	b.AddSource("owner@example.com")
	base := time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)
	legacyEmail := b.AddMessage(MessageOpt{
		Subject: "legacy email", LegacyEmptyMessageType: true, SentAt: base,
	})
	typedEmail := b.AddMessage(MessageOpt{
		Subject: "typed email", MessageType: "email", SentAt: base.Add(time.Hour),
	})
	sms := b.AddMessage(MessageOpt{
		Snippet: "sms text", MessageType: messageTypeSMS,
		ConversationType: "direct_chat", SentAt: base.Add(2 * time.Hour),
	})
	engine := b.BuildEngine()

	anchorIDs := func(response *ExploreResponse) []int64 {
		ids := make([]int64, 0, len(response.Rows))
		for _, row := range response.Rows {
			require.NotNil(row.AnchorMessageID, "row %s must carry an anchor", row.Key)
			ids = append(ids, *row.AnchorMessageID)
		}
		return ids
	}

	emailFast, emailLegacyPath := runExploreBothPaths(t, engine, ExploreRequest{
		Context: Context{MessageTypes: []string{"email"}}, Page: PageSpec{Limit: 50},
	})
	assert.Equal(emailLegacyPath, emailFast)
	assert.Equal(int64(2), emailFast.TotalCount)
	assert.ElementsMatch([]int64{legacyEmail, typedEmail}, anchorIDs(emailFast))

	smsFast, smsLegacyPath := runExploreBothPaths(t, engine, ExploreRequest{
		Context: Context{MessageTypes: []string{messageTypeSMS}}, Page: PageSpec{Limit: 50},
	})
	assert.Equal(smsLegacyPath, smsFast)
	assert.Equal(int64(1), smsFast.TotalCount)
	assert.ElementsMatch([]int64{sms}, anchorIDs(smsFast))
}

func TestExploreCoverageStreamsExactLiveMessagesInOneScan(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	selected := b.AddSource("selected@example.com")
	other := b.AddSource("other@example.com")
	deletedAt := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)

	const liveCount = 600
	for range liveCount {
		b.AddMessage(MessageOpt{SourceID: selected, MessageType: "email"})
	}
	b.AddMessage(MessageOpt{SourceID: selected, MessageType: "email", InternalDeletedAt: &deletedAt})
	b.AddMessage(MessageOpt{SourceID: selected, MessageType: "email", DeletedFromSourceAt: &deletedAt})
	b.AddMessage(MessageOpt{SourceID: other, MessageType: "email"})
	engine := b.BuildEngine()

	var got []int64
	batchCalls := 0
	result, err := engine.ExploreCoverage(context.Background(), ExploreCoverageRequest{
		Context:   Context{SourceIDs: []int64{selected}},
		BatchSize: 128,
	}, func(messageIDs []int64) error {
		batchCalls++
		assert.LessOrEqual(len(messageIDs), 128)
		got = append(got, messageIDs...)
		return nil
	})
	require.NoError(err)

	assert.Greater(batchCalls, 1)
	assert.Equal(int64(liveCount), result.EligibleCount)
	assert.NotEmpty(result.CacheRevision)
	require.Len(got, liveCount)
	for i, id := range got {
		assert.Equal(int64(i+1), id)
	}
}

func TestExploreCoverageStopsOnVisitError(t *testing.T) {
	require := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("selected@example.com")
	for range 5 {
		b.AddMessage(MessageOpt{SourceID: source, MessageType: "email"})
	}
	engine := b.BuildEngine()

	visitErr := errors.New("intersection backend failed")
	visitCalls := 0
	_, err := engine.ExploreCoverage(context.Background(), ExploreCoverageRequest{BatchSize: 2},
		func([]int64) error {
			visitCalls++
			return visitErr
		})
	require.ErrorIs(err, visitErr)
	require.Equal(1, visitCalls)
}

func TestExploreCoverageHonorsCancellation(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSource("selected@example.com")
	b.AddMessage(MessageOpt{SourceID: source})
	engine := b.BuildEngine()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := engine.ExploreCoverage(ctx, ExploreCoverageRequest{BatchSize: 10}, func([]int64) error { return nil })
	require.ErrorIs(t, err, context.Canceled)
}

func assertExploreBoundedStrongestMatch(t *testing.T, row EntryRow, want *int64) {
	t.Helper()
	assertions := assert.New(t)
	rowType := reflect.TypeFor[EntryRow]()
	_, hasArchiveSizedIDs := rowType.FieldByName("MatchedMessageIDs")
	assertions.False(hasArchiveSizedIDs, "logical rows must not retain every constituent message ID")
	assertions.Equal(want, row.StrongestMatchedMessageID)
}

func TestExploreLogicalRowUnitsAndStableArchiveOrdering(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	gmail := b.AddSourceWithType("archive-a@example.com", "gmail")
	maildir := b.AddSourceWithType("archive-b@example.com", "imap")
	chat := b.AddSourceWithType("+15550000000", "imessage")
	calendar := b.AddSourceWithType("calendar@example.com", "google_calendar")
	meeting := b.AddSourceWithType("meetings@example.com", "granola")
	other := b.AddSourceWithType("items@example.com", "durable_source")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice")
	bob := b.AddParticipant("bob@example.com", "example.com", "Bob")
	phone := b.AddPhoneParticipant("+15550000001", "Test Contact")

	equalTime := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	firstEmail := b.AddMessage(MessageOpt{SourceID: gmail, Subject: "Archive A", SentAt: equalTime, MessageType: "email"})
	b.AddFrom(firstEmail, alice, "Alice")
	secondEmail := b.AddMessage(MessageOpt{SourceID: maildir, Subject: "Archive B", SentAt: equalTime, MessageType: "email"})
	b.AddFrom(secondEmail, bob, "Bob")
	b.AddAttachment(secondEmail, 42, "agenda.pdf")

	const chatConversationID = int64(900)
	for i := range 10_000 {
		messageID := b.AddMessage(MessageOpt{
			SourceID: chat, ConversationID: chatConversationID,
			ConversationType: "direct_chat", ConversationTitle: "Synthetic chat",
			MessageType: "imessage", SenderID: &phone,
			SentAt: equalTime.Add(-time.Duration(i+1) * time.Second),
		})
		b.AddFrom(messageID, phone, "Test Contact")
	}

	event := b.AddMessage(MessageOpt{SourceID: calendar, MessageType: "calendar_event", ConversationType: "calendar", Subject: "Planning", SentAt: equalTime.Add(-time.Hour)})
	b.AddFrom(event, alice, "Alice")
	note := b.AddMessage(MessageOpt{SourceID: meeting, MessageType: "meeting_transcript", ConversationType: "meeting", Subject: "Weekly notes", SentAt: equalTime.Add(-2 * time.Hour)})
	b.AddFrom(note, bob, "Bob")
	item := b.AddMessage(MessageOpt{SourceID: other, MessageType: "bookmark", ConversationType: "items", Subject: "Durable item", SentAt: equalTime.Add(-3 * time.Hour)})
	b.AddFrom(item, alice, "Alice")
	deletedAt := equalTime.Add(-4 * time.Hour)
	deleted := b.AddMessage(MessageOpt{SourceID: gmail, MessageType: "email", Subject: "Deleted at source", SentAt: deletedAt, DeletedFromSourceAt: &deletedAt})
	b.AddFrom(deleted, alice, "Alice")

	engine := b.BuildEngine()
	response, err := engine.Explore(context.Background(), ExploreRequest{Page: PageSpec{Limit: 20}})
	require.NoError(err)
	require.Len(response.Rows, 7)
	assert.NotEmpty(response.CacheRevision)
	assert.Equal([]EntryKind{
		EntryEmail, EntryEmail, EntryConversation, EntryEvent, EntryMeeting, EntryItem, EntryEmail,
	}, entryKinds(response.Rows))
	assert.Equal("Archive A", response.Rows[0].Title)
	assert.Equal("Archive B", response.Rows[1].Title)
	assert.NotEqual(response.Rows[0].Key, response.Rows[1].Key, "equal timestamps across archives need distinct stable keys")
	assert.Equal(int64(10_000), response.Rows[2].MessageCount)
	assert.True(response.Rows[1].HasAttachments)
	assert.Equal(int64(1), response.Rows[1].AttachmentCount)
	assert.True(response.Rows[6].DeletedFromSource)
}

func TestExploreFlattensTitleOnlyWhenFallenBackToSnippet(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	gmail := b.AddSourceWithType("archive-a@example.com", "gmail")
	when := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	withSubject := b.AddMessage(MessageOpt{
		SourceID: gmail, Subject: "Re: 2 ** 3 == 8?", Snippet: "### Meeting notes\n- Action item", SentAt: when,
	})
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice")
	b.AddFrom(withSubject, alice, "Alice")
	fallback := b.AddMessage(MessageOpt{
		SourceID: gmail, Subject: "", Snippet: "### Meeting notes\n- Action item", SentAt: when.Add(-time.Minute),
	})
	b.AddFrom(fallback, alice, "Alice")

	engine := b.BuildEngine()
	response, err := engine.Explore(context.Background(), ExploreRequest{Page: PageSpec{Limit: 20}})
	require.NoError(err)
	require.Len(response.Rows, 2)
	assert.Equal("Re: 2 ** 3 == 8?", response.Rows[0].Title, "a real subject with markdown-like characters must not be altered")
	assert.Equal("Meeting notes Action item", response.Rows[1].Title, "a title that fell back to the snippet is still flattened")
}

func TestExploreAppliesContextBeforeChatAggregation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	chat := b.AddSourceWithType("+15550000000", "sms")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice")
	bob := b.AddParticipant("bob@other.test", "other.test", "Bob")
	const conversationID = int64(901)
	for i := range 3 {
		sender := alice
		if i == 2 {
			sender = bob
		}
		id := b.AddMessage(MessageOpt{SourceID: chat, ConversationID: conversationID, ConversationType: "group_chat", MessageType: "sms", SenderID: &sender})
		b.AddFrom(id, sender, "")
	}

	engine := b.BuildEngine()
	response, err := engine.Explore(context.Background(), ExploreRequest{
		Context: Context{ParticipantIDs: []int64{alice}}, Page: PageSpec{Limit: 10},
	})
	require.NoError(err)
	require.Len(response.Rows, 1)
	assert.Equal(EntryConversation, response.Rows[0].Kind)
	assert.Equal(int64(2), response.Rows[0].MessageCount, "chat facts must be computed after context filtering")
}

func TestExploreKeepsDurableCallsAsIndividualItemsInsideChatConversations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	voice := b.AddSourceWithType("voice@example.com", "google_voice")
	caller := b.AddPhoneParticipant("+15550000002", "Test Caller")
	const conversationID = int64(902)
	for _, messageType := range []string{
		"google_voice_call",
		"google_voice_voicemail",
		"synctech_sms_call",
	} {
		b.AddMessage(MessageOpt{
			SourceID: voice, ConversationID: conversationID,
			ConversationType: "direct_chat", MessageType: messageType,
			SenderID: &caller,
		})
	}

	response, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{})
	require.NoError(err)
	require.Len(response.Rows, 3, "durable call and voicemail records must not collapse into a chat row")
	for _, row := range response.Rows {
		assert.Equal(EntryItem, row.Kind)
		assert.Equal(int64(1), row.MessageCount)
	}
}

func TestExploreAggregatesRCSFragmentsIntoOneConversation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	rcsSource := b.AddSourceWithType("rcs@example.com", "synctech_sms")
	sender := b.AddPhoneParticipant("+15550000006", "RCS Contact")
	const conversationID = int64(904)
	for range 3 {
		b.AddMessage(MessageOpt{
			SourceID: rcsSource, ConversationID: conversationID,
			ConversationType: "direct_chat", ConversationTitle: "RCS chat",
			MessageType: "rcs", SenderID: &sender,
		})
	}

	response, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{})
	require.NoError(err)
	require.Len(response.Rows, 1)
	assert.Equal(EntryConversation, response.Rows[0].Kind)
	assert.Equal(int64(3), response.Rows[0].MessageCount)
}

func TestExploreIncludesDirectSendersAndConversationMembership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	beeper := b.AddSourceWithType("beeper@example.com", "beeper")
	alice := b.AddPhoneParticipant("+15550000003", "Alice")
	bob := b.AddPhoneParticipant("+15550000004", "Bob")
	carol := b.AddParticipant("carol@members.example", "members.example", "Carol")
	const conversationID = int64(903)
	for i := range 2 {
		sender := alice
		if i == 1 {
			sender = bob
		}
		b.AddMessage(MessageOpt{
			SourceID: beeper, ConversationID: conversationID,
			ConversationType: "direct_chat", ConversationTitle: "Beeper chat",
			MessageType: "beeper", SenderID: &sender,
		})
	}
	b.AddConversationParticipant(conversationID, alice)
	b.AddConversationParticipant(conversationID, bob)
	b.AddConversationParticipant(conversationID, carol)

	response, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{})
	require.NoError(err)
	require.Len(response.Rows, 1)
	assert.Equal([]int64{alice, bob, carol}, response.Rows[0].ParticipantIDs)
	assert.Equal([]string{"Alice", "Bob", "Carol"}, response.Rows[0].ParticipantLabels)

	memberResponse, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{
		Context: Context{ParticipantIDs: []int64{carol}},
	})
	require.NoError(err)
	require.Len(memberResponse.Rows, 1, "conversation membership must participate in person context")

	domainResponse, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{
		Context: Context{Domains: []string{"members.example"}},
	})
	require.NoError(err)
	require.Len(domainResponse.Rows, 1, "conversation membership domains must participate in domain context")
}

func TestExploreReturnsSearchIndexProvenance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	sourceID := b.AddSource("archive@example.com")
	messageID := b.AddMessage(MessageOpt{SourceID: sourceID, Subject: "Needle"})
	engine := b.BuildEngine()
	generation := int64(17)

	tests := []struct {
		name        string
		search      SearchSpec
		wantLexical string
		wantVector  *int64
	}{
		{name: "none", search: SearchSpec{}},
		{
			name: "full text",
			search: SearchSpec{
				Mode: SearchFullText, Query: "needle", CandidateMessageIDs: []int64{messageID},
				LexicalIndexRevision: "fts5:23",
			},
			wantLexical: "fts5:23",
		},
		{
			name: "semantic",
			search: SearchSpec{
				Mode: SearchSemantic, Query: "needle", CandidateMessageIDs: []int64{messageID},
				VectorGeneration: &generation,
			},
			wantVector: &generation,
		},
		{
			name: "hybrid",
			search: SearchSpec{
				Mode: SearchHybrid, Query: "needle", CandidateMessageIDs: []int64{messageID},
				LexicalIndexRevision: "fts5:23", VectorGeneration: &generation,
			},
			wantLexical: "fts5:23", wantVector: &generation,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, err := engine.Explore(context.Background(), ExploreRequest{Search: tt.search})
			require.NoError(err)
			assert.Equal(tt.wantLexical, response.SearchProvenance.LexicalIndexRevision)
			assert.Equal(tt.wantVector, response.SearchProvenance.VectorGeneration)
		})
	}
}

func TestExploreRejectsModeInapplicableSearchFields(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSource("archive@example.com")
	messageID := b.AddMessage(MessageOpt{SourceID: sourceID, Subject: "Needle"})
	engine := b.BuildEngine()
	generation := int64(17)

	tests := []struct {
		name   string
		search SearchSpec
	}{
		{name: "none query", search: SearchSpec{Query: "needle"}},
		{name: "none explicit empty candidates", search: SearchSpec{CandidateMessageIDs: []int64{}}},
		{name: "none nonempty candidates", search: SearchSpec{CandidateMessageIDs: []int64{messageID}}},
		{name: "none lexical revision", search: SearchSpec{LexicalIndexRevision: "fts5:23"}},
		{name: "none vector generation", search: SearchSpec{VectorGeneration: &generation}},
		{
			name: "full text vector generation",
			search: SearchSpec{
				Mode: SearchFullText, Query: "needle", CandidateMessageIDs: []int64{messageID},
				LexicalIndexRevision: "fts5:23", VectorGeneration: &generation,
			},
		},
		{
			name: "semantic lexical revision",
			search: SearchSpec{
				Mode: SearchSemantic, Query: "needle", CandidateMessageIDs: []int64{messageID},
				LexicalIndexRevision: "fts5:23", VectorGeneration: &generation,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := engine.Explore(context.Background(), ExploreRequest{Search: tt.search})
			require.ErrorIs(t, err, ErrInvalidExploreRequest)
		})
	}
}

func TestExploreEmptyAnalyticsDirReturnsTypedAbsentCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	_, err := (&DuckDBEngine{}).Explore(context.Background(), ExploreRequest{})
	require.Error(err)
	require.ErrorIs(err, ErrCacheUnavailable)
	var unavailable *CacheUnavailableError
	require.ErrorAs(err, &unavailable)
	assert.Equal(CacheAbsent, unavailable.Readiness)
}

func TestExploreRejectsUnresolvedSearchCandidatesAndProvenance(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSource("archive@example.com")
	b.AddMessage(MessageOpt{SourceID: sourceID, Subject: "Needle"})
	engine := b.BuildEngine()
	generation := int64(23)

	tests := []struct {
		name   string
		search SearchSpec
	}{
		{
			name: "full text candidates unresolved",
			search: SearchSpec{Mode: SearchFullText,
				LexicalIndexRevision: "fts5:24"},
		},
		{
			name: "full text lexical revision unresolved",
			search: SearchSpec{Mode: SearchFullText,
				CandidateMessageIDs: []int64{}},
		},
		{
			name: "semantic candidates unresolved",
			search: SearchSpec{Mode: SearchSemantic,
				VectorGeneration: &generation},
		},
		{
			name: "semantic generation unresolved",
			search: SearchSpec{Mode: SearchSemantic,
				CandidateMessageIDs: []int64{}},
		},
		{
			name: "hybrid candidates unresolved",
			search: SearchSpec{Mode: SearchHybrid,
				LexicalIndexRevision: "fts5:24", VectorGeneration: &generation},
		},
		{
			name: "hybrid lexical revision unresolved",
			search: SearchSpec{Mode: SearchHybrid,
				CandidateMessageIDs: []int64{}, VectorGeneration: &generation},
		},
		{
			name: "hybrid generation unresolved",
			search: SearchSpec{Mode: SearchHybrid,
				CandidateMessageIDs: []int64{}, LexicalIndexRevision: "fts5:24"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := engine.Explore(context.Background(), ExploreRequest{Search: tt.search})
			require.ErrorIs(t, err, ErrInvalidExploreRequest)
		})
	}
}

func TestExploreResolvedEmptySearchCandidatesReturnNoRows(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSource("archive@example.com")
	b.AddMessage(MessageOpt{SourceID: sourceID, Subject: "Needle"})
	engine := b.BuildEngine()
	generation := int64(23)

	searches := []SearchSpec{
		{Mode: SearchFullText, CandidateMessageIDs: []int64{}, LexicalIndexRevision: "fts5:24"},
		{Mode: SearchSemantic, CandidateMessageIDs: []int64{}, VectorGeneration: &generation},
		{Mode: SearchHybrid, CandidateMessageIDs: []int64{}, LexicalIndexRevision: "fts5:24", VectorGeneration: &generation},
	}
	for _, search := range searches {
		response, err := engine.Explore(context.Background(), ExploreRequest{Search: search})
		require.NoError(t, err)
		assert.Empty(t, response.Rows)
		assert.Zero(t, response.TotalCount)
	}
}

func TestExploreRejectsUnsupportedPresentationAndSort(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSource("archive@example.com")
	b.AddMessage(MessageOpt{SourceID: sourceID})
	engine := b.BuildEngine()

	tests := []struct {
		name    string
		request ExploreRequest
	}{
		{name: "timeline presentation", request: ExploreRequest{Presentation: PresentationTimeline}},
		{name: "ascending date", request: ExploreRequest{Sort: []SortSpec{{Field: "sent_at", Direction: "asc"}}}},
		{name: "unsupported field", request: ExploreRequest{Sort: []SortSpec{{Field: "message_count", Direction: "desc"}}}},
		{name: "multiple sorts", request: ExploreRequest{Sort: []SortSpec{{Field: "sent_at", Direction: "desc"}, {Field: "source_id", Direction: "asc"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := engine.Explore(context.Background(), tt.request)
			require.ErrorIs(t, err, ErrInvalidExploreRequest)
		})
	}
}

func TestExploreAcceptsExplicitTableDateDescending(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSource("archive@example.com")
	b.AddMessage(MessageOpt{SourceID: sourceID})

	response, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{
		Presentation: Presentation("table"),
		Sort:         []SortSpec{{Field: "sent_at", Direction: "desc"}},
	})
	require.NoError(t, err)
	assert.Len(t, response.Rows, 1)
}

func TestExplorePreservesTotalCountBeyondLastPage(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSource("archive@example.com")
	b.AddMessage(MessageOpt{SourceID: sourceID, Subject: "Only row"})
	engine := b.BuildEngine()

	response, err := engine.Explore(context.Background(), ExploreRequest{Page: PageSpec{Limit: 1, Offset: 10}})
	require.NoError(t, err)
	assert.Empty(t, response.Rows)
	assert.Equal(t, int64(1), response.TotalCount)
}

func entryKinds(rows []EntryRow) []EntryKind {
	kinds := make([]EntryKind, len(rows))
	for i := range rows {
		kinds[i] = rows[i].Kind
	}
	return kinds
}

// TestExploreCounterpartParticipantIDExcludesOwner verifies
// counterpart_participant_id is the smallest NON-owner participant on the
// entry, not simply participant_ids[0]: the owner is added first (so it has
// the smallest raw ID) and would be picked by that naive heuristic.
func TestExploreCounterpartParticipantIDExcludesOwner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)
	counterpartA := b.AddParticipant("alice@example.com", "example.com", "Alice")
	counterpartB := b.AddParticipant("bob@example.com", "example.com", "Bob")

	msgID := b.AddMessage(MessageOpt{SourceID: srcID, IsFromMe: true})
	b.AddFrom(msgID, ownerID, "Owner")
	b.AddTo(msgID, counterpartA, "Alice")
	b.AddTo(msgID, counterpartB, "Bob")

	response, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{})
	require.NoError(err)
	require.Len(response.Rows, 1)
	require.NotNil(response.Rows[0].CounterpartParticipantID)
	assert.Equal(min(counterpartA, counterpartB), *response.Rows[0].CounterpartParticipantID)
}

// TestExploreCounterpartParticipantIDNilWhenOwnerOnly verifies an entry whose
// only participant is the owner (e.g. a self-addressed note) yields a nil
// counterpart rather than falling back to the owner's own ID.
func TestExploreCounterpartParticipantIDNilWhenOwnerOnly(t *testing.T) {
	require := require.New(t)
	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)

	msgID := b.AddMessage(MessageOpt{SourceID: srcID, IsFromMe: true})
	b.AddFrom(msgID, ownerID, "Owner")
	b.AddTo(msgID, ownerID, "Owner")

	response, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{})
	require.NoError(err)
	require.Len(response.Rows, 1)
	assert.Nil(t, response.Rows[0].CounterpartParticipantID)
}

// TestExploreCounterpartSkipsOwnerIdentityFromAnotherSource pins the
// person-level owner semantics for counterpart selection in a multi-source
// archive (see buildExploreSQL / buildRelationshipsSQL): an address confirmed
// as an owner identity on source A is never "the other side" of a source-B
// entry. The source-A identity has the smallest raw participant ID, so
// source-scoped owner filtering would regress to picking it. Cross-account
// self-mail with no third participant must yield a nil counterpart, not the
// owner's other address.
func TestExploreCounterpartSkipsOwnerIdentityFromAnotherSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	srcA := b.AddSource("owner@personal.example")
	srcB := b.AddSource("owner@work.example")
	personalID := b.AddParticipant("owner@personal.example", "personal.example", "Owner Personal")
	workID := b.AddParticipant("owner@work.example", "work.example", "Owner Work")
	b.AddOwnerParticipant(srcA, personalID)
	b.AddOwnerParticipant(srcB, workID)
	bobID := b.AddParticipant("bob@example.com", "example.com", "Bob")

	forwardID := b.AddMessage(MessageOpt{SourceID: srcB, Subject: "Forwarded with Bob"})
	b.AddFrom(forwardID, personalID, "Owner Personal")
	b.AddTo(forwardID, workID, "Owner Work")
	b.AddTo(forwardID, bobID, "Bob")

	selfMailID := b.AddMessage(MessageOpt{SourceID: srcB, Subject: "Note to self"})
	b.AddFrom(selfMailID, personalID, "Owner Personal")
	b.AddTo(selfMailID, workID, "Owner Work")

	response, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{
		Context: Context{SourceIDs: []int64{srcB}},
	})
	require.NoError(err)
	require.Len(response.Rows, 2)

	counterpartsBySubject := make(map[string]*int64, len(response.Rows))
	for _, row := range response.Rows {
		counterpartsBySubject[row.Title] = row.CounterpartParticipantID
	}
	require.Contains(counterpartsBySubject, "Forwarded with Bob")
	require.NotNil(counterpartsBySubject["Forwarded with Bob"])
	assert.Equal(bobID, *counterpartsBySubject["Forwarded with Bob"],
		"the source-A owner identity must be skipped even though it has the smallest participant ID")
	require.Contains(counterpartsBySubject, "Note to self")
	assert.Nil(counterpartsBySubject["Note to self"],
		"cross-account self-mail has no counterpart")
}

// TestExploreCounterpartParticipantIDNilWhenOwnerUnknown verifies that when
// no owner_participants rows exist at all (the owner set is unknown), the
// column is nil rather than guessing the smallest participant ID overall —
// the exact heuristic this field replaces.
func TestExploreCounterpartParticipantIDNilWhenOwnerUnknown(t *testing.T) {
	require := require.New(t)
	b := NewTestDataBuilder(t)
	srcID := b.AddSource("archive@example.com")
	senderID := b.AddParticipant("alice@example.com", "example.com", "Alice")
	recipientID := b.AddParticipant("bob@example.com", "example.com", "Bob")

	msgID := b.AddMessage(MessageOpt{SourceID: srcID})
	b.AddFrom(msgID, senderID, "Alice")
	b.AddTo(msgID, recipientID, "Bob")

	response, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{})
	require.NoError(err)
	require.Len(response.Rows, 1)
	assert.Nil(t, response.Rows[0].CounterpartParticipantID)
}
