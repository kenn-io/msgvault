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
