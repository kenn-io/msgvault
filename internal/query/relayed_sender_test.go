package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func TestDuckDBRelayedSenderEnvelope(t *testing.T) {
	b := NewTestDataBuilder(t)
	b.AddSource("reader@example.test")
	author := b.AddParticipant("", "", "alice")
	message := b.AddMessage(MessageOpt{Subject: "Notice", SenderID: &author})
	b.AddRecipientWithEnvelope(message, author, "from", "GitHub", "notifications@github.com")
	dir, cleanup := b.Build()
	t.Cleanup(cleanup)
	engine, err := NewDuckDBEngine(t.Context(), dir, "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	results, err := engine.SearchFast(t.Context(), search.Parse("from:notifications@github.com"), MessageFilter{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "notifications@github.com", results[0].FromEmail)
	rows, err := engine.ListMessages(t.Context(), MessageFilter{Sender: "notifications@github.com", Domain: "github.com"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "notifications@github.com", rows[0].FromEmail)
	domains, err := engine.Aggregate(t.Context(), ViewDomains, DefaultAggregateOptions())
	require.NoError(t, err)
	require.Len(t, domains, 1)
	assert.Equal(t, "github.com", domains[0].Key)
}
