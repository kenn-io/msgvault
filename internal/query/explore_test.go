package query

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSharedChatClassificationSQL(t *testing.T) {
	engine, err := NewDuckDBEngine("", "", nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, engine.Close())
	})

	tests := []struct {
		name             string
		messageType      string
		conversationType string
		want             bool
	}{
		{name: "email thread", messageType: "email", conversationType: "email_thread", want: false},
		{name: "known text type", messageType: "iMessage", conversationType: "email_thread", want: true},
		{name: "fallback chat type", messageType: "", conversationType: "group_chat", want: true},
		{name: "fallback outside chat", messageType: "text", conversationType: "email_thread", want: false},
		{name: "unknown type in chat", messageType: "unknown", conversationType: "direct_chat", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			var sqlResult bool
			err := engine.db.QueryRow(
				"SELECT "+sqlIsChatPredicate("message_type", "conversation_type")+
					" FROM (VALUES (?::VARCHAR, ?::VARCHAR)) AS input(message_type, conversation_type)",
				test.messageType,
				test.conversationType,
			).Scan(&sqlResult)
			requirements.NoError(err)
			assertions.Equal(test.want, identityindex.IsChat(test.messageType, test.conversationType))
			assertions.Equal(test.want, IsChatEntry(test.messageType, test.conversationType))
			assertions.Equal(test.want, sqlResult)
		})
	}
}

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

func TestExploreIdentityDirectionSeparatesSenderAndRecipientWithinSource(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	sourceA := b.AddSource("archive-a@example.com")
	sourceB := b.AddSource("archive-b@example.com")
	identityID := b.AddParticipant("identity@example.com", "example.com", "Identity")
	otherID := b.AddParticipant("other@example.net", "example.net", "Other")
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	sent := b.AddMessage(MessageOpt{SourceID: sourceA, Subject: "sent", SentAt: base})
	b.AddFrom(sent, otherID, "Other")
	b.AddFrom(sent, identityID, "Identity")
	b.AddTo(sent, otherID, "Other")
	received := b.AddMessage(MessageOpt{SourceID: sourceA, Subject: "received", SentAt: base.Add(-time.Hour)})
	b.AddFrom(received, otherID, "Other")
	b.AddTo(received, identityID, "Identity")
	otherSource := b.AddMessage(MessageOpt{SourceID: sourceB, Subject: "other source", SentAt: base.Add(time.Hour)})
	b.AddFrom(otherSource, identityID, "Identity")

	engine := b.BuildEngine()
	explore := func(direction IdentityDirection) *ExploreResponse {
		result, err := engine.Explore(context.Background(), ExploreRequest{
			Context: Context{Identity: &IdentityPredicate{
				SourceID: sourceA, ParticipantIDs: []int64{identityID}, Direction: direction,
			}},
			Page: PageSpec{Limit: 10},
		})
		requirements.NoError(err)
		return result
	}

	sender := explore(IdentityDirectionSender)
	requirements.Len(sender.Rows, 1)
	assertions.Equal("sent", sender.Rows[0].Title)
	assertions.Equal(sourceA, sender.Rows[0].SourceID)
	assertions.Equal(int64(1), sender.TotalCount)

	recipient := explore(IdentityDirectionRecipient)
	requirements.Len(recipient.Rows, 1)
	assertions.Equal("received", recipient.Rows[0].Title)
	assertions.Equal(sourceA, recipient.Rows[0].SourceID)
	assertions.Equal(int64(1), recipient.TotalCount)

	anyDirection := explore(IdentityDirectionAny)
	requirements.Len(anyDirection.Rows, 2)
	assertions.Equal([]string{"sent", "received"}, []string{anyDirection.Rows[0].Title, anyDirection.Rows[1].Title})
	for _, row := range anyDirection.Rows {
		assertions.Equal(sourceA, row.SourceID, "identity predicate must not leak to another source")
	}
}

func TestExploreIdentitySenderUsesDirectSenderOnlyWithoutFromRows(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	identityID := b.AddParticipant("identity@example.com", "example.com", "Identity")
	explicitFromID := b.AddParticipant("explicit@example.net", "example.net", "Explicit")
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	multiFrom := b.AddMessage(MessageOpt{SourceID: source, Subject: "multi from", SenderID: &explicitFromID, SentAt: base})
	b.AddFrom(multiFrom, explicitFromID, "Explicit")
	b.AddFrom(multiFrom, identityID, "Identity")
	directConflict := b.AddMessage(MessageOpt{SourceID: source, Subject: "direct conflict", SenderID: &identityID, SentAt: base.Add(-time.Hour)})
	b.AddFrom(directConflict, explicitFromID, "Explicit")
	b.AddMessage(MessageOpt{SourceID: source, Subject: "direct fallback", SenderID: &identityID, SentAt: base.Add(-2 * time.Hour)})

	result, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{
		Context: Context{Identity: &IdentityPredicate{
			SourceID: source, ParticipantIDs: []int64{identityID}, Direction: IdentityDirectionSender,
		}},
		Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 2)
	assertions.Equal([]string{"multi from", "direct fallback"}, []string{result.Rows[0].Title, result.Rows[1].Title})
	assertions.Equal(int64(2), result.TotalCount)
}

func TestExploreIdentityPredicateAppliesBeforeChatAggregation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("+15550000000", messageTypeIMessage)
	identityID := b.AddParticipant("identity@example.com", "example.com", "Identity")
	otherID := b.AddParticipant("other@example.net", "example.net", "Other")
	const conversationID = int64(812)

	sent := b.AddMessage(MessageOpt{SourceID: source, ConversationID: conversationID, MessageType: messageTypeIMessage, ConversationType: "direct_chat", SenderID: &identityID})
	b.AddFrom(sent, identityID, "Identity")
	b.AddTo(sent, otherID, "Other")
	received := b.AddMessage(MessageOpt{SourceID: source, ConversationID: conversationID, MessageType: messageTypeIMessage, ConversationType: "direct_chat", SenderID: &otherID})
	b.AddFrom(received, otherID, "Other")
	b.AddCc(received, identityID, "Identity")

	engine := b.BuildEngine()
	for _, test := range []struct {
		direction IdentityDirection
		wantCount int64
	}{
		{direction: IdentityDirectionSender, wantCount: 1},
		{direction: IdentityDirectionRecipient, wantCount: 1},
		{direction: IdentityDirectionAny, wantCount: 2},
	} {
		result, err := engine.Explore(context.Background(), ExploreRequest{
			Context: Context{Identity: &IdentityPredicate{
				SourceID: source, ParticipantIDs: []int64{identityID}, Direction: test.direction,
			}},
			Page: PageSpec{Limit: 10},
		})
		requirements.NoError(err, test.direction)
		requirements.Len(result.Rows, 1, test.direction)
		assertions.Equal(EntryConversation, result.Rows[0].Kind, test.direction)
		assertions.Equal(test.wantCount, result.Rows[0].MessageCount, test.direction)
	}
}

func TestExploreIdentityPredicateValidatesSourceIDsParticipantsAndDirection(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	identityID := b.AddParticipant("identity@example.com", "example.com", "Identity")
	messageID := b.AddMessage(MessageOpt{SourceID: source})
	b.AddFrom(messageID, identityID, "Identity")
	engine := b.BuildEngine()

	tests := []struct {
		name      string
		context   Context
		wantError string
	}{
		{name: "missing source", context: Context{Identity: &IdentityPredicate{ParticipantIDs: []int64{identityID}, Direction: IdentityDirectionAny}}, wantError: "source ID"},
		{name: "missing participant IDs", context: Context{Identity: &IdentityPredicate{SourceID: source, Direction: IdentityDirectionAny}}, wantError: "participant IDs"},
		{name: "match none with participant IDs", context: Context{Identity: &IdentityPredicate{SourceID: source, ParticipantIDs: []int64{identityID}, MatchNone: true, Direction: IdentityDirectionAny}}, wantError: "match-none"},
		{name: "zero participant ID", context: Context{Identity: &IdentityPredicate{SourceID: source, ParticipantIDs: []int64{0}, Direction: IdentityDirectionAny}}, wantError: "participant ID"},
		{name: "invalid direction", context: Context{Identity: &IdentityPredicate{SourceID: source, ParticipantIDs: []int64{identityID}, Direction: "sideways"}}, wantError: "identity direction"},
		{name: "mismatched source", context: Context{SourceIDs: []int64{source + 1}, Identity: &IdentityPredicate{SourceID: source, ParticipantIDs: []int64{identityID}, Direction: IdentityDirectionAny}}, wantError: "source IDs"},
		{name: "multiple sources", context: Context{SourceIDs: []int64{source, source + 1}, Identity: &IdentityPredicate{SourceID: source, ParticipantIDs: []int64{identityID}, Direction: IdentityDirectionAny}}, wantError: "source IDs"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			_, err := engine.Explore(context.Background(), ExploreRequest{Context: test.context})
			requirements.Error(err)
			requirements.ErrorContains(err, test.wantError)
			assertions.ErrorIs(err, ErrInvalidExploreRequest)
		})
	}

	valid, err := engine.Explore(context.Background(), ExploreRequest{Context: Context{
		Identity: &IdentityPredicate{SourceID: source, ParticipantIDs: []int64{identityID}, Direction: IdentityDirectionAny},
	}})
	requirements.NoError(err, "the source-pinned predicate is valid without a redundant SourceIDs field")
	assertions.Equal(int64(1), valid.TotalCount)

	matchingSource, err := engine.Explore(context.Background(), ExploreRequest{Context: Context{
		SourceIDs: []int64{source},
		Identity:  &IdentityPredicate{SourceID: source, ParticipantIDs: []int64{identityID}, Direction: IdentityDirectionAny},
	}})
	requirements.NoError(err)
	assertions.Equal(int64(1), matchingSource.TotalCount)

	matchNone, err := engine.Explore(context.Background(), ExploreRequest{Context: Context{
		Identity: &IdentityPredicate{SourceID: source, MatchNone: true, Direction: IdentityDirectionAny},
	}})
	requirements.NoError(err, "a confirmed identity without observed participants is a valid empty filter")
	assertions.Zero(matchNone.TotalCount)
	assertions.Empty(matchNone.Rows)
}

// TestExploreIdentityFilterPrefersEnvelopeAddressOverMergedParticipant models
// the post-merge archive: one surviving participant row backs mail sent
// through two different envelope addresses. Filtering on one alias must
// select only that alias's envelope rows, not everything the survivor
// participant touches.
func TestExploreIdentityFilterPrefersEnvelopeAddressOverMergedParticipant(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	survivorID := b.AddParticipant("alias-b@example.com", "example.com", "Survivor")
	otherID := b.AddParticipant("other@example.net", "example.net", "Other")
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	sentViaA := b.AddMessage(MessageOpt{SourceID: source, Subject: "sent via a", SentAt: base})
	b.AddRecipientWithEnvelope(sentViaA, survivorID, "from", "Survivor", "alias-a@example.com")
	b.AddTo(sentViaA, otherID, "Other")
	sentViaB := b.AddMessage(MessageOpt{SourceID: source, Subject: "sent via b", SentAt: base.Add(-time.Hour)})
	b.AddRecipientWithEnvelope(sentViaB, survivorID, "from", "Survivor", "alias-b@example.com")
	receivedAtA := b.AddMessage(MessageOpt{SourceID: source, Subject: "received at a", SentAt: base.Add(-2 * time.Hour)})
	b.AddFrom(receivedAtA, otherID, "Other")
	b.AddRecipientWithEnvelope(receivedAtA, survivorID, "to", "Survivor", "alias-a@example.com")
	receivedAtB := b.AddMessage(MessageOpt{SourceID: source, Subject: "received at b", SentAt: base.Add(-3 * time.Hour)})
	b.AddFrom(receivedAtB, otherID, "Other")
	b.AddRecipientWithEnvelope(receivedAtB, survivorID, "cc", "Survivor", "alias-b@example.com")

	engine := b.BuildEngine()
	explore := func(direction IdentityDirection, emailIdentifier string) *ExploreResponse {
		result, err := engine.Explore(context.Background(), ExploreRequest{
			Context: Context{Identity: &IdentityPredicate{
				SourceID:        source,
				ParticipantIDs:  []int64{survivorID},
				EmailIdentifier: emailIdentifier,
				Direction:       direction,
			}},
			Page: PageSpec{Limit: 10},
		})
		requirements.NoError(err)
		return result
	}

	sender := explore(IdentityDirectionSender, "alias-a@example.com")
	requirements.Len(sender.Rows, 1, "alias A sender filter must not match alias B's envelope")
	assertions.Equal("sent via a", sender.Rows[0].Title)

	recipient := explore(IdentityDirectionRecipient, "alias-a@example.com")
	requirements.Len(recipient.Rows, 1, "alias A recipient filter must not match alias B's envelope")
	assertions.Equal("received at a", recipient.Rows[0].Title)

	anyDirection := explore(IdentityDirectionAny, "ALIAS-A@EXAMPLE.COM")
	requirements.Len(anyDirection.Rows, 2, "envelope comparison stays case-insensitive for emails")
	assertions.Equal(int64(2), anyDirection.TotalCount)

	participantOnly := explore(IdentityDirectionAny, "")
	assertions.Equal(int64(4), participantOnly.TotalCount,
		"without an email identifier the participant rules stay in force")
}

// TestExploreIdentityFilterFallsBackToParticipantForRowsWithoutEnvelope locks
// in the legacy contract on the analytical path: recipient rows whose
// envelope snapshot is empty keep matching through the resolved participant
// IDs even when the filter carries an email identifier.
func TestExploreIdentityFilterFallsBackToParticipantForRowsWithoutEnvelope(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	identityID := b.AddParticipant("identity@example.com", "example.com", "Identity")
	otherID := b.AddParticipant("other@example.net", "example.net", "Other")
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	legacySent := b.AddMessage(MessageOpt{SourceID: source, Subject: "legacy sent", SentAt: base})
	b.AddFrom(legacySent, identityID, "Identity")
	unrelated := b.AddMessage(MessageOpt{SourceID: source, Subject: "unrelated", SentAt: base.Add(-time.Hour)})
	b.AddFrom(unrelated, otherID, "Other")
	directChat := b.AddMessage(MessageOpt{SourceID: source, Subject: "direct chat", SenderID: &identityID, SentAt: base.Add(-2 * time.Hour)})
	_ = directChat

	result, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{
		Context: Context{Identity: &IdentityPredicate{
			SourceID:        source,
			ParticipantIDs:  []int64{identityID},
			EmailIdentifier: "identity@example.com",
			Direction:       IdentityDirectionSender,
		}},
		Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 2)
	assertions.Equal([]string{"legacy sent", "direct chat"},
		[]string{result.Rows[0].Title, result.Rows[1].Title},
		"empty-envelope rows and direct senders keep the participant fallback")
}

// TestExploreIdentitySenderSuppressesFallbackWhenAnyEnvelopeExists mirrors
// attribution's message-level rule on the analytical path: one populated From
// envelope makes the envelope authoritative for the whole message, so a
// coexisting legacy NULL From row must not select the message through the
// participant fallback when attribution stored it as not-from-me.
func TestExploreIdentitySenderSuppressesFallbackWhenAnyEnvelopeExists(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	identityID := b.AddParticipant("identity@example.com", "example.com", "Identity")
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	mixed := b.AddMessage(MessageOpt{SourceID: source, Subject: "mixed envelopes", SentAt: base})
	b.AddFrom(mixed, identityID, "Identity")
	b.AddRecipientWithEnvelope(mixed, identityID, "from", "Outside", "outside@example.net")
	legacy := b.AddMessage(MessageOpt{SourceID: source, Subject: "legacy only", SentAt: base.Add(-time.Hour)})
	b.AddFrom(legacy, identityID, "Identity")

	result, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{
		Context: Context{Identity: &IdentityPredicate{
			SourceID:        source,
			ParticipantIDs:  []int64{identityID},
			EmailIdentifier: "identity@example.com",
			Direction:       IdentityDirectionSender,
		}},
		Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 1,
		"a populated From envelope suppresses the sender participant fallback message-wide")
	assertions.Equal("legacy only", result.Rows[0].Title)
}

// TestExploreIdentityFilterMatchesEnvelopeWithoutParticipantEvidence covers
// the merge edge where no participant row carries the alias any more (the
// absorbed email vanished from every participant surface): the envelope
// snapshot alone must still select the alias's mail.
func TestExploreIdentityFilterMatchesEnvelopeWithoutParticipantEvidence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	survivorID := b.AddParticipant("alias-b@example.com", "example.com", "Survivor")
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	sentViaA := b.AddMessage(MessageOpt{SourceID: source, Subject: "sent via a", SentAt: base})
	b.AddRecipientWithEnvelope(sentViaA, survivorID, "from", "Survivor", "alias-a@example.com")
	sentViaB := b.AddMessage(MessageOpt{SourceID: source, Subject: "sent via b", SentAt: base.Add(-time.Hour)})
	b.AddRecipientWithEnvelope(sentViaB, survivorID, "from", "Survivor", "alias-b@example.com")
	directOnly := b.AddMessage(MessageOpt{SourceID: source, Subject: "direct only", SenderID: &survivorID, SentAt: base.Add(-2 * time.Hour)})
	_ = directOnly

	result, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{
		Context: Context{Identity: &IdentityPredicate{
			SourceID:        source,
			EmailIdentifier: "alias-a@example.com",
			Direction:       IdentityDirectionAny,
		}},
		Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err, "an envelope-only email identity is a valid predicate without participant IDs")
	requirements.Len(result.Rows, 1)
	assertions.Equal("sent via a", result.Rows[0].Title,
		"only the alias's own envelope rows match; no participant fallback exists to widen the result")
}

// TestExploreIdentityFilterLegacyCacheWithoutEnvelopeColumnFallsBack proves a
// committed cache written before the envelope column existed keeps serving
// identity filters with the participant semantics instead of erroring.
func TestExploreIdentityFilterLegacyCacheWithoutEnvelopeColumnFallsBack(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	b.SetLegacyRecipientSchema()
	source := b.AddSource("archive@example.com")
	identityID := b.AddParticipant("identity@example.com", "example.com", "Identity")
	otherID := b.AddParticipant("other@example.net", "example.net", "Other")
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	sent := b.AddMessage(MessageOpt{SourceID: source, Subject: "legacy cache sent", SentAt: base})
	b.AddFrom(sent, identityID, "Identity")
	unrelated := b.AddMessage(MessageOpt{SourceID: source, Subject: "unrelated", SentAt: base.Add(-time.Hour)})
	b.AddFrom(unrelated, otherID, "Other")

	result, err := b.BuildEngine().Explore(context.Background(), ExploreRequest{
		Context: Context{Identity: &IdentityPredicate{
			SourceID:        source,
			ParticipantIDs:  []int64{identityID},
			EmailIdentifier: "identity@example.com",
			Direction:       IdentityDirectionSender,
		}},
		Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err, "a pre-v17 cache without the envelope column must not error")
	requirements.Len(result.Rows, 1)
	assertions.Equal("legacy cache sent", result.Rows[0].Title)
}

func TestExploreIdentityPredicateScopesCountsGroupsSelectionAndSearchCandidates(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	identityID := b.AddParticipant("identity@example.com", "example.com", "Identity")
	otherID := b.AddParticipant("other@example.net", "example.net", "Other")
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	matchingFirst := b.AddMessage(MessageOpt{SourceID: source, Subject: "matching first", SentAt: base})
	b.AddFrom(matchingFirst, identityID, "Identity")
	b.AddAttachment(matchingFirst, 10, "first.txt")
	matchingSecond := b.AddMessage(MessageOpt{SourceID: source, Subject: "matching second", SentAt: base.Add(-time.Hour)})
	b.AddFrom(matchingSecond, identityID, "Identity")
	nonmatching := b.AddMessage(MessageOpt{SourceID: source, Subject: "nonmatching", SentAt: base.Add(-2 * time.Hour)})
	b.AddFrom(nonmatching, otherID, "Other")
	b.AddAttachment(nonmatching, 20, "other.txt")
	engine := b.BuildEngine()
	identityContext := Context{Identity: &IdentityPredicate{
		SourceID: source, ParticipantIDs: []int64{identityID}, Direction: IdentityDirectionSender,
	}}

	page, err := engine.Explore(context.Background(), ExploreRequest{Context: identityContext, Page: PageSpec{Limit: 1}})
	requirements.NoError(err)
	requirements.Len(page.Rows, 1)
	assertions.Equal("matching first", page.Rows[0].Title)
	assertions.Equal(int64(2), page.TotalCount)

	offset, err := engine.Explore(context.Background(), ExploreRequest{Context: identityContext, Page: PageSpec{Limit: 1, Offset: 1}})
	requirements.NoError(err)
	requirements.Len(offset.Rows, 1)
	assertions.Equal("matching second", offset.Rows[0].Title)
	assertions.Equal(int64(2), offset.TotalCount)

	grouped, err := engine.ExploreGroups(context.Background(), ExploreGroupRequest{
		Explore: ExploreRequest{Context: identityContext}, Dimension: "year",
		Sort: SortSpec{Field: "count", Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(grouped.Rows, 1)
	assertions.Equal(int64(2), grouped.Rows[0].Count)

	selection, err := engine.ExploreSelectionStats(context.Background(), ExploreSelectionRequest{
		Explore: ExploreRequest{Context: identityContext},
	})
	requirements.NoError(err)
	assertions.Equal(int64(2), selection.Count)
	assertions.Equal(int64(1), selection.FileCount)

	exploreFiles, err := engine.ExploreFiles(context.Background(), ExploreFilesRequest{
		Explore: ExploreRequest{Context: identityContext}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(exploreFiles.Files, 1)
	assertions.Equal("first.txt", exploreFiles.Files[0].Filename)
	assertions.Equal(int64(1), exploreFiles.TotalCount)

	generation := int64(7)
	for _, test := range []struct {
		name   string
		search SearchSpec
	}{
		{name: "full text", search: SearchSpec{Mode: SearchFullText, Query: "match", CandidateMessageIDs: []int64{matchingFirst, nonmatching}, LexicalIndexRevision: "fts5:test"}},
		{name: "semantic", search: SearchSpec{Mode: SearchSemantic, Query: "match", CandidateMessageIDs: []int64{matchingFirst, nonmatching}, VectorGeneration: &generation}},
		{name: "hybrid", search: SearchSpec{Mode: SearchHybrid, Query: "match", CandidateMessageIDs: []int64{matchingFirst, nonmatching}, LexicalIndexRevision: "fts5:test", VectorGeneration: &generation}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, searchErr := engine.Explore(context.Background(), ExploreRequest{
				Context: identityContext, Search: test.search, Page: PageSpec{Limit: 10},
			})
			require.NoError(t, searchErr)
			require.Len(t, result.Rows, 1)
			assert.Equal(t, matchingFirst, *result.Rows[0].AnchorMessageID)
		})
	}

	matchCounts, err := engine.ExploreMatchCounts(context.Background(), ExploreMatchCountsRequest{
		Explore: ExploreRequest{Context: identityContext, Search: SearchSpec{
			Mode: SearchFullText, Query: "match", CandidateMessageIDs: []int64{matchingFirst, nonmatching}, LexicalIndexRevision: "fts5:test",
		}},
		RowKeys: []string{"source:1:message:msg1", "source:1:message:msg3"},
	})
	requirements.NoError(err)
	assertions.Equal(map[string]int64{"source:1:message:msg1": 1, "source:1:message:msg3": 0}, matchCounts.Counts)
	assertions.NotEqual(matchingSecond, nonmatching)
}

func TestExploreIdentityPredicateScopesRelationshipTimeline(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	sourceA := b.AddSource("archive-a@example.com")
	sourceB := b.AddSource("archive-b@example.com")
	identityID := b.AddParticipant("identity@example.com", "example.com", "Identity")
	otherID := b.AddParticipant("other@example.net", "example.net", "Other")
	counterpartID := b.AddParticipant("counterpart@example.org", "example.org", "Counterpart")
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	matching := b.AddMessage(MessageOpt{SourceID: sourceA, Subject: "matching", SentAt: base})
	b.AddFrom(matching, identityID, "Identity")
	b.AddTo(matching, counterpartID, "Counterpart")
	nonmatching := b.AddMessage(MessageOpt{SourceID: sourceA, Subject: "nonmatching", SentAt: base.Add(-time.Hour)})
	b.AddFrom(nonmatching, otherID, "Other")
	b.AddTo(nonmatching, counterpartID, "Counterpart")
	otherSource := b.AddMessage(MessageOpt{SourceID: sourceB, Subject: "other source", SentAt: base.Add(time.Hour)})
	b.AddFrom(otherSource, identityID, "Identity")
	b.AddTo(otherSource, counterpartID, "Counterpart")

	result, err := b.BuildEngine().RelationshipTimeline(context.Background(), RelationshipTimelineRequest{
		CanonicalID: counterpartID,
		Context: Context{SourceIDs: []int64{sourceA}, Identity: &IdentityPredicate{
			SourceID: sourceA, ParticipantIDs: []int64{identityID}, Direction: IdentityDirectionSender,
		}},
		Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 1)
	assertions.Equal("matching", result.Rows[0].Title)
	assertions.Equal(sourceA, result.Rows[0].SourceID)
	assertions.Equal(int64(1), result.TotalCount)
}

func TestExploreIdentityPredicateScopesPeopleGrouping(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	identityID := b.AddParticipant("identity@example.com", "example.com", "Identity")
	otherID := b.AddParticipant("other@example.net", "example.net", "Other")
	counterpartID := b.AddParticipant("counterpart@example.org", "example.org", "Counterpart")

	matching := b.AddMessage(MessageOpt{SourceID: source, Subject: "matching"})
	b.AddFrom(matching, identityID, "Identity")
	b.AddTo(matching, counterpartID, "Counterpart")
	nonmatching := b.AddMessage(MessageOpt{SourceID: source, Subject: "nonmatching"})
	b.AddFrom(nonmatching, otherID, "Other")
	b.AddTo(nonmatching, counterpartID, "Counterpart")

	result, err := b.BuildEngine().SearchPeople(context.Background(), PersonSearchRequest{
		Explore: ExploreRequest{Context: Context{Identity: &IdentityPredicate{
			SourceID: source, ParticipantIDs: []int64{identityID}, Direction: IdentityDirectionSender,
		}}},
		Sort: SortSpec{Field: "activity_count", Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 2)
	assertions.ElementsMatch([]int64{identityID, counterpartID}, []int64{result.Rows[0].ID, result.Rows[1].ID})
	assertions.NotContains([]int64{result.Rows[0].ID, result.Rows[1].ID}, otherID)
	assertions.Equal(int64(2), result.TotalCount)
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
// archive (see buildExploreSQL and the relationship index): an address confirmed
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

// TestExploreCoverageParticipantFilterMatchesExploreAcrossClusters guards that
// the coverage population widens a participant filter across the whole identity
// cluster, so the coverage message-ID set for a canonical participant filter
// matches Explore's set for the same filter — both include alias-owned
// messages. Before the fix, coverage omitted alias-only messages and disagreed
// with Explore.
func TestExploreCoverageParticipantFilterMatchesExploreAcrossClusters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	canonical := b.AddParticipant("alice@example.com", "example.com", "Alice")
	alias := b.AddParticipant("alice@work.example", "work.example", "Alice (Work)")
	b.LinkCluster(canonical, alias)

	when := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	toCanonical := b.AddMessage(MessageOpt{SourceID: source, MessageType: "email", SentAt: when})
	b.AddTo(toCanonical, canonical, "Alice")
	toAliasFirst := b.AddMessage(MessageOpt{SourceID: source, MessageType: "email", SentAt: when.Add(time.Hour)})
	b.AddTo(toAliasFirst, alias, "Alice (Work)")
	toAliasSecond := b.AddMessage(MessageOpt{SourceID: source, MessageType: "email", SentAt: when.Add(2 * time.Hour)})
	b.AddTo(toAliasSecond, alias, "Alice (Work)")
	engine := b.BuildEngine()
	ctx := context.Background()

	filter := Context{ParticipantIDs: []int64{canonical}}

	var coverageIDs []int64
	_, err := engine.ExploreCoverage(ctx, ExploreCoverageRequest{Context: filter, BatchSize: 10},
		func(messageIDs []int64) error {
			coverageIDs = append(coverageIDs, messageIDs...)
			return nil
		})
	require.NoError(err)
	assert.Equal([]int64{toCanonical, toAliasFirst, toAliasSecond}, coverageIDs,
		"coverage must include alias-owned messages under a canonical-ID filter")

	explored, err := engine.Explore(ctx, ExploreRequest{Context: filter, Page: PageSpec{Limit: 10}})
	require.NoError(err)
	exploreIDs := make([]int64, 0, len(explored.Rows))
	for _, row := range explored.Rows {
		require.NotNil(row.AnchorMessageID)
		exploreIDs = append(exploreIDs, *row.AnchorMessageID)
	}
	assert.ElementsMatch(coverageIDs, exploreIDs,
		"coverage and Explore must resolve the same message set for the same filter")
}

// TestExploreAdditionalParticipantGroupsIntersectRatherThanUnion proves that
// drilling a co-participant group (B) under an existing participant filter
// (A) narrows to entries involving BOTH A and B, instead of widening to
// every entry that involves either.
func TestExploreAdditionalParticipantGroupsIntersectRatherThanUnion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice")
	bob := b.AddParticipant("bob@example.com", "example.com", "Bob")
	carol := b.AddParticipant("carol@example.com", "example.com", "Carol")

	when := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	onlyAlice := b.AddMessage(MessageOpt{SourceID: source, MessageType: "email", SentAt: when})
	b.AddFrom(onlyAlice, alice, "Alice")
	b.AddTo(onlyAlice, carol, "Carol")
	onlyBob := b.AddMessage(MessageOpt{SourceID: source, MessageType: "email", SentAt: when.Add(time.Hour)})
	b.AddFrom(onlyBob, bob, "Bob")
	b.AddTo(onlyBob, carol, "Carol")
	both := b.AddMessage(MessageOpt{SourceID: source, MessageType: "email", SentAt: when.Add(2 * time.Hour)})
	b.AddFrom(both, alice, "Alice")
	b.AddTo(both, bob, "Bob")
	engine := b.BuildEngine()
	ctx := context.Background()

	response, err := engine.Explore(ctx, ExploreRequest{
		Context: Context{ParticipantIDs: []int64{alice}, AdditionalParticipantGroups: [][]int64{{bob}}},
		Page:    PageSpec{Limit: 10},
	})
	require.NoError(err)
	require.Len(response.Rows, 1, "only the entry involving both Alice and Bob must match the conjunction")
	require.NotNil(response.Rows[0].AnchorMessageID)
	assert.Equal(both, *response.Rows[0].AnchorMessageID)
	assert.Equal(int64(1), response.TotalCount)

	aliceOnly, err := engine.Explore(ctx, ExploreRequest{
		Context: Context{ParticipantIDs: []int64{alice}}, Page: PageSpec{Limit: 10},
	})
	require.NoError(err)
	assert.Len(aliceOnly.Rows, 2, "sanity check: the base Alice filter alone matches both Alice entries")
}

// TestExploreAdditionalDomainGroupsIntersectRatherThanUnion is the domain
// analogue of TestExploreAdditionalParticipantGroupsIntersectRatherThanUnion.
func TestExploreAdditionalDomainGroupsIntersectRatherThanUnion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	alice := b.AddParticipant("alice@domain-a.example", "domain-a.example", "Alice")
	bob := b.AddParticipant("bob@domain-b.example", "domain-b.example", "Bob")
	carol := b.AddParticipant("carol@domain-c.example", "domain-c.example", "Carol")

	when := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	onlyA := b.AddMessage(MessageOpt{SourceID: source, MessageType: "email", SentAt: when})
	b.AddFrom(onlyA, alice, "Alice")
	b.AddTo(onlyA, carol, "Carol")
	onlyB := b.AddMessage(MessageOpt{SourceID: source, MessageType: "email", SentAt: when.Add(time.Hour)})
	b.AddFrom(onlyB, bob, "Bob")
	b.AddTo(onlyB, carol, "Carol")
	both := b.AddMessage(MessageOpt{SourceID: source, MessageType: "email", SentAt: when.Add(2 * time.Hour)})
	b.AddFrom(both, alice, "Alice")
	b.AddTo(both, bob, "Bob")
	engine := b.BuildEngine()
	ctx := context.Background()

	response, err := engine.Explore(ctx, ExploreRequest{
		Context: Context{
			Domains:                []string{"domain-a.example"},
			AdditionalDomainGroups: [][]string{{"domain-b.example"}},
		},
		Page: PageSpec{Limit: 10},
	})
	require.NoError(err)
	require.Len(response.Rows, 1, "only the entry touching both domains must match the conjunction")
	require.NotNil(response.Rows[0].AnchorMessageID)
	assert.Equal(both, *response.Rows[0].AnchorMessageID)
}

// TestExploreAdditionalParticipantGroupExpandsThroughIdentityClusters proves
// that a conjunctive drill-down group is identity-cluster expanded exactly
// like the primary participant filter: a non-canonical alias in the
// additional group still matches entries recorded under its canonical
// identity.
func TestExploreAdditionalParticipantGroupExpandsThroughIdentityClusters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice")
	bobCanonical := b.AddParticipant("bob@example.com", "example.com", "Bob")
	bobAlias := b.AddParticipant("bob@work.example", "work.example", "Bob (Work)")
	b.LinkCluster(bobCanonical, bobAlias)
	carol := b.AddParticipant("carol@example.com", "example.com", "Carol")

	when := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	onlyAlice := b.AddMessage(MessageOpt{SourceID: source, MessageType: "email", SentAt: when})
	b.AddFrom(onlyAlice, alice, "Alice")
	b.AddTo(onlyAlice, carol, "Carol")
	// This entry involves Bob's CANONICAL identity, never the alias.
	aliceAndBobCanonical := b.AddMessage(MessageOpt{SourceID: source, MessageType: "email", SentAt: when.Add(time.Hour)})
	b.AddFrom(aliceAndBobCanonical, alice, "Alice")
	b.AddTo(aliceAndBobCanonical, bobCanonical, "Bob")
	engine := b.BuildEngine()
	ctx := context.Background()

	// Filtering by Alice AND the alias must still match the entry recorded
	// against Bob's canonical identity.
	response, err := engine.Explore(ctx, ExploreRequest{
		Context: Context{ParticipantIDs: []int64{alice}, AdditionalParticipantGroups: [][]int64{{bobAlias}}},
		Page:    PageSpec{Limit: 10},
	})
	require.NoError(err)
	require.Len(response.Rows, 1, "the additional group must widen the alias to its whole identity cluster")
	require.NotNil(response.Rows[0].AnchorMessageID)
	assert.Equal(aliceAndBobCanonical, *response.Rows[0].AnchorMessageID)
}
