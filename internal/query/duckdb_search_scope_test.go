package query

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func TestDuckDBSearchFast_RecipientMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	b.AddSource("test@example.com")
	senderID := b.AddParticipant("sender@example.com", "example.com", "Sender")
	recipientID := b.AddParticipant("recipientneedle@example.com", "example.com", "Recipient Needle")
	messageID := b.AddMessage(MessageOpt{Subject: "ordinary subject", Snippet: "ordinary preview"})
	b.AddFrom(messageID, senderID, "Sender")
	b.AddTo(messageID, recipientID, "Recipient Needle")
	engine := b.BuildEngine()

	q := &search.Query{TextTerms: []string{"recipientneedle"}}
	messages, err := engine.SearchFast(context.Background(), q, MessageFilter{}, 50, 0)
	require.NoError(err, "SearchFast")
	require.Len(messages, 1, "recipient metadata hit")
	assert.Equal(messageID, messages[0].ID, "recipient metadata hit ID")

	count, err := engine.SearchFastCount(context.Background(), q, MessageFilter{})
	require.NoError(err, "SearchFastCount")
	assert.Equal(int64(len(messages)), count, "result/count agreement")
}

func TestDuckDBSearchFast_AllFromParticipantsMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	b.AddSource("test@example.com")
	primaryFromID := b.AddParticipant("primary@example.com", "example.com", "Primary Sender")
	secondaryFromID := b.AddParticipant("secondaryneedle@example.com", "example.com", "Secondary Needle")
	messageID := b.AddMessage(MessageOpt{Subject: "ordinary subject", Snippet: "ordinary preview"})
	b.AddFrom(messageID, primaryFromID, "Primary Sender")
	b.AddFrom(messageID, secondaryFromID, "Secondary Needle")
	engine := b.BuildEngine()

	q := &search.Query{TextTerms: []string{"secondaryneedle"}}
	messages, err := engine.SearchFast(context.Background(), q, MessageFilter{}, 50, 0)
	require.NoError(err, "SearchFast")
	require.Len(messages, 1, "secondary from participant metadata hit")
	assert.Equal(messageID, messages[0].ID, "secondary from participant hit ID")

	count, err := engine.SearchFastCount(context.Background(), q, MessageFilter{})
	require.NoError(err, "SearchFastCount")
	assert.Equal(int64(len(messages)), count, "result/count agreement")
}

func TestDuckDBSearchFast_UnscopedIncludesCachedMessageTypes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	b := NewTestDataBuilder(t)
	b.AddSource("test@example.com")

	messageTypes := []string{
		messageTypeEmail,
		messageTypeMeetingTranscript,
		messageTypeSMS,
		"whatsapp",
		"imessage",
		"teams",
	}
	wantIDs := make([]int64, 0, len(messageTypes))
	var wantSize int64
	for i, messageType := range messageTypes {
		size := int64((i + 1) * 100)
		wantIDs = append(wantIDs, b.AddMessage(MessageOpt{
			Subject:      "cachewide needle " + messageType,
			MessageType:  messageType,
			SizeEstimate: size,
		}))
		wantSize += size
	}
	b.AddMessage(MessageOpt{
		Subject:      "unrelated email",
		MessageType:  messageTypeEmail,
		SizeEstimate: 9999,
	})
	b.SetEmptyAttachments()
	engine := b.BuildEngine()
	ctx := context.Background()

	q := search.Parse("cachewide needle")
	messages, err := engine.SearchFast(ctx, q, MessageFilter{}, 50, 0)
	require.NoError(err, "SearchFast")
	assertMessageIDs(t, messages, wantIDs)

	count, err := engine.SearchFastCount(ctx, q, MessageFilter{})
	require.NoError(err, "SearchFastCount")
	assert.Equal(int64(len(wantIDs)), count, "unscoped result/count agreement")

	result, err := engine.SearchFastWithStats(ctx, q, "cachewide needle", MessageFilter{}, ViewSenders, 50, 0)
	require.NoError(err, "SearchFastWithStats")
	assertMessageIDs(t, result.Messages, wantIDs)
	assert.Equal(int64(len(wantIDs)), result.TotalCount, "combined total count")
	require.NotNil(result.Stats, "combined stats")
	assert.Equal(int64(len(wantIDs)), result.Stats.MessageCount, "combined stats count")
	assert.Equal(wantSize, result.Stats.TotalSize, "combined stats size")

	queryScoped, err := engine.SearchFast(
		ctx,
		search.Parse("message_type:meeting_transcript cachewide needle"),
		MessageFilter{},
		50,
		0,
	)
	require.NoError(err, "SearchFast query-scoped")
	require.Len(queryScoped, 1, "query message type remains authoritative")
	assert.Equal(messageTypeMeetingTranscript, queryScoped[0].MessageType)
	explicitResult, err := engine.SearchFastWithStats(
		ctx,
		search.Parse("message_type:meeting_transcript cachewide needle"),
		"message_type:meeting_transcript cachewide needle",
		MessageFilter{},
		ViewSenders,
		50,
		0,
	)
	require.NoError(err, "SearchFastWithStats explicit scope after unscoped cache")
	require.Len(explicitResult.Messages, 1, "explicit cached message scope")
	assert.Equal(int64(1), explicitResult.TotalCount, "explicit cached count")
	require.NotNil(explicitResult.Stats, "explicit cached stats")
	assert.Equal(int64(1), explicitResult.Stats.MessageCount, "explicit cached stats count")

	filterScoped, err := engine.SearchFast(
		ctx,
		q,
		MessageFilter{MessageType: messageTypeSMS},
		50,
		0,
	)
	require.NoError(err, "SearchFast filter-scoped")
	require.Len(filterScoped, 1, "filter message type remains authoritative")
	assert.Equal(messageTypeSMS, filterScoped[0].MessageType)

	conflictingQuery := search.Parse("message_type:meeting_transcript cachewide needle")
	conflictingFilter := MessageFilter{MessageType: messageTypeSMS}
	conflicting, err := engine.SearchFast(ctx, conflictingQuery, conflictingFilter, 50, 0)
	require.NoError(err, "SearchFast conflicting scopes")
	assert.Empty(conflicting, "conflicting explicit scopes")

	conflictingCount, err := engine.SearchFastCount(ctx, conflictingQuery, conflictingFilter)
	require.NoError(err, "SearchFastCount conflicting scopes")
	assert.Zero(conflictingCount, "conflicting explicit scope count")

	conflictingResult, err := engine.SearchFastWithStats(
		ctx,
		conflictingQuery,
		"message_type:meeting_transcript cachewide needle",
		conflictingFilter,
		ViewSenders,
		50,
		0,
	)
	require.NoError(err, "SearchFastWithStats conflicting scopes after explicit cache")
	assert.Empty(conflictingResult.Messages, "conflicting cached messages")
	assert.Zero(conflictingResult.TotalCount, "conflicting cached count")
	require.NotNil(conflictingResult.Stats, "conflicting cached stats")
	assert.Zero(conflictingResult.Stats.MessageCount, "conflicting cached stats count")
}

func TestDuckDBSearchMessageBodies_DelegatesToDirectSQLite(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.EnableFTS()
	engine := &DuckDBEngine{sqliteEngine: env.Engine}

	messages, err := engine.SearchMessageBodies(context.Background(),
		&search.Query{TextTerms: []string{"body 1"}}, 50, 0)
	require.NoError(err, "SearchMessageBodies")
	require.Len(messages, 1, "body-only hit")
	assert.Equal(int64(1), messages[0].ID, "body-only hit ID")
	require.NotEmpty(messages[0].BodyContextSnippets, "body-only hit context")
	assert.Contains(messages[0].BodyContextSnippets[0], "body 1")
}

func TestDuckDBSearchMessageBodies_RequiresDirectSQLite(t *testing.T) {
	_, err := (&DuckDBEngine{}).SearchMessageBodies(context.Background(),
		&search.Query{TextTerms: []string{"needle"}}, 50, 0)
	require.Error(t, err, "missing direct SQLite engine")
	require.ErrorIs(t, err, ErrMessageBodySearchUnavailable)
	assert.Contains(t, err.Error(), "direct SQLite engine")
}
