package query_test

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
	"time"
)

func TestQuotedLocalPartSenderDomain(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "quoted", "")
	require.NoError(t, err)
	sender, err := st.EnsureParticipant("primary@example.test", "Author", "example.test")
	require.NoError(t, err)
	_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:    &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "quoted", MessageType: "email", SenderID: sql.NullInt64{Int64: sender, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}},
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, EmailAddresses: []string{`"a@b"@relay.example.test`}}},
	})
	require.NoError(t, err)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	domains, err := engine.Aggregate(t.Context(), query.ViewDomains, query.DefaultAggregateOptions())
	require.NoError(t, err)
	require.Len(t, domains, 1)
	assert.Equal(t, "relay.example.test", domains[0].Key)
	rows, err := engine.ListMessages(t.Context(), query.MessageFilter{Domain: "relay.example.test"})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}
