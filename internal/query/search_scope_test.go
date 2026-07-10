package query_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestSearchFast_MetadataOnly runs against SQLite by default and PostgreSQL in
// the test-pg lane. It exercises the production query engine and real FTS
// index so body terms cannot accidentally leak back into the metadata path.
func TestSearchFast_MetadataOnly(t *testing.T) {
	rootRequire := require.New(t)
	rootAssert := assert.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	senderID := f.EnsureParticipant("senderneedle@example.com", "Sender Needle", "example.com")
	recipientID := f.EnsureParticipant("recipientneedle@example.com", "Recipient Needle", "example.com")

	subjectID := createSearchScopeMessage(t, f, "scope-subject", "subjectneedle update", "ordinary preview", "ordinary body", 0, 0)
	snippetID := createSearchScopeMessage(t, f, "scope-snippet", "ordinary subject", "snippetneedle preview", "ordinary body", 0, 0)
	senderMessageID := createSearchScopeMessage(t, f, "scope-sender", "ordinary subject", "ordinary preview", "ordinary body", senderID, 0)
	recipientMessageID := createSearchScopeMessage(t, f, "scope-recipient", "ordinary subject", "ordinary preview", "ordinary body", 0, recipientID)
	bodyOnlyID := createSearchScopeMessage(t, f, "scope-body", "ordinary subject", "ordinary preview", "bodyneedle appears only in the body", 0, 0)

	_, err := f.Store.BackfillFTS(nil)
	rootRequire.NoError(err, "BackfillFTS")

	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}

	tests := []struct {
		name string
		term string
		want []int64
	}{
		{name: "subject", term: "subjectneedle", want: []int64{subjectID}},
		{name: "snippet", term: "snippetneedle", want: []int64{snippetID}},
		{name: "sender", term: "senderneedle", want: []int64{senderMessageID}},
		{name: "recipient", term: "recipientneedle", want: []int64{recipientMessageID}},
		{name: "body_only", term: "bodyneedle", want: []int64{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			q := &search.Query{TextTerms: []string{tc.term}}
			messages, err := engine.SearchFast(ctx, q, query.MessageFilter{}, 50, 0)
			require.NoError(err, "SearchFast")

			gotIDs := make([]int64, len(messages))
			for i := range messages {
				gotIDs[i] = messages[i].ID
			}
			assert.ElementsMatch(tc.want, gotIDs, "SearchFast IDs")

			count, err := engine.SearchFastCount(ctx, q, query.MessageFilter{})
			require.NoError(err, "SearchFastCount")
			assert.Equal(int64(len(messages)), count, "result/count agreement")
		})
	}

	// The generic search contract remains composite and must still find body
	// text after the metadata-only fast path is corrected.
	messages, err := engine.Search(ctx, &search.Query{TextTerms: []string{"bodyneedle"}}, 50, 0)
	rootRequire.NoError(err, "Search")
	rootRequire.Len(messages, 1, "generic Search body hit")
	rootAssert.Equal(bodyOnlyID, messages[0].ID, "generic Search body hit ID")
}

func TestSearchFastWithStats_MetadataPredicateConsistency(t *testing.T) {
	rootRequire := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	metadataID := createSearchScopeMessage(t, f, "scope-stats-metadata",
		"metadatastatneedle subject", "ordinary preview", "ordinary body", 0, 0)
	createSearchScopeMessage(t, f, "scope-stats-body",
		"ordinary subject", "ordinary preview", "bodystatneedle appears only in the body", 0, 0)
	_, err := f.Store.BackfillFTS(nil)
	rootRequire.NoError(err, "BackfillFTS")

	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}

	tests := []struct {
		name    string
		term    string
		wantIDs []int64
	}{
		{name: "body-only term", term: "bodystatneedle", wantIDs: []int64{}},
		{name: "metadata term", term: "metadatastatneedle", wantIDs: []int64{metadataID}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			result, err := engine.SearchFastWithStats(ctx,
				&search.Query{TextTerms: []string{tc.term}}, tc.term,
				query.MessageFilter{}, query.ViewSenders, 50, 0)
			require.NoError(err, "SearchFastWithStats")
			require.NotNil(result.Stats, "search stats")

			gotIDs := make([]int64, len(result.Messages))
			for i := range result.Messages {
				gotIDs[i] = result.Messages[i].ID
			}
			assert.ElementsMatch(tc.wantIDs, gotIDs, "message IDs")
			assert.Equal(int64(len(tc.wantIDs)), result.TotalCount, "total count")
			assert.Equal(result.TotalCount, result.Stats.MessageCount,
				"stats must describe the same metadata-only matches")
		})
	}
}

// TestSearchMessageBodies_BodyColumnOnly runs against SQLite by default and
// PostgreSQL in the test-pg lane. Every non-body FTS field carries the same
// term in a different message, proving the optional capability scopes the
// index to the body column/weight instead of returning composite hits.
func TestSearchMessageBodies_BodyColumnOnly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	fromID := f.EnsureParticipant("scopeword@example.com", "Scopeword Sender", "example.com")
	toID := f.EnsureParticipant("scopeword-to@example.com", "Scopeword To", "example.com")
	ccID := f.EnsureParticipant("scopeword-cc@example.com", "Scopeword Cc", "example.com")

	bodyID := createSearchScopeMessage(t, f, "body-scope-body", "ordinary subject", "ordinary preview", "scopeword appears in the body", 0, 0)
	createSearchScopeMessage(t, f, "body-scope-subject", "scopeword subject", "ordinary preview", "ordinary body", 0, 0)
	fromMessageID := createSearchScopeMessage(t, f, "body-scope-from", "ordinary subject", "ordinary preview", "ordinary body", fromID, 0)
	toMessageID := createSearchScopeMessage(t, f, "body-scope-to", "ordinary subject", "ordinary preview", "ordinary body", 0, toID)
	ccMessageID := createSearchScopeMessage(t, f, "body-scope-cc", "ordinary subject", "ordinary preview", "ordinary body", 0, 0)
	require.NoError(f.Store.ReplaceMessageRecipients(
		ccMessageID, "cc", []int64{ccID}, []string{"Scopeword Cc"}), "ReplaceMessageRecipients(cc)")

	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}

	messages, err := engine.SearchMessageBodies(ctx, &search.Query{TextTerms: []string{"scopeword"}}, 50, 0)
	require.NoError(err, "SearchMessageBodies")
	require.Len(messages, 1, "body-only hits")
	assert.Equal(bodyID, messages[0].ID, "body-only hit ID")

	nonBodyIDs := []int64{fromMessageID, toMessageID, ccMessageID}
	for _, id := range nonBodyIDs {
		assert.NotEqual(id, messages[0].ID, "metadata-only FTS field must not cross into body scope")
	}
}

func TestSearchMessageBodies_PostgreSQLRejectsStaleLayout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL indexing_version readiness contract")
	}
	messageID := createSearchScopeMessage(t, f, "body-scope-stale", "ordinary subject", "ordinary preview", "stalecheck body", 0, 0)
	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")
	assert.False(f.Store.NeedsFTSBackfill(), "fresh layout is ready")

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		"UPDATE messages SET indexing_version = ? WHERE id = ?"),
		store.CurrentFTSIndexingVersion-1, messageID)
	require.NoError(err, "mark layout stale")
	assert.True(f.Store.NeedsFTSBackfill(), "stale version needs backfill")

	engine := query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	_, err = engine.SearchMessageBodies(context.Background(),
		&search.Query{TextTerms: []string{"stalecheck"}}, 50, 0)
	require.Error(err, "stale body search must fail closed")
	require.ErrorIs(err, query.ErrMessageBodySearchIndexStale)
	assert.Contains(err.Error(), "rebuild-fts")
	assert.Contains(err.Error(), "backfill")
}

func createSearchScopeMessage(
	t *testing.T,
	f *storetest.Fixture,
	sourceMessageID, subject, snippet, body string,
	senderID, recipientID int64,
) int64 {
	t.Helper()

	messageID := f.NewMessage().
		WithSourceMessageID(sourceMessageID).
		WithSubject(subject).
		WithSnippet(snippet).
		Create(t, f.Store)
	require.NoError(t, f.Store.UpsertMessageBody(messageID,
		sql.NullString{String: body, Valid: true}, sql.NullString{}), "UpsertMessageBody")
	if senderID != 0 {
		require.NoError(t, f.Store.ReplaceMessageRecipients(
			messageID, "from", []int64{senderID}, []string{"Sender Needle"}), "ReplaceMessageRecipients(from)")
	}
	if recipientID != 0 {
		require.NoError(t, f.Store.ReplaceMessageRecipients(
			messageID, "to", []int64{recipientID}, []string{"Recipient Needle"}), "ReplaceMessageRecipients(to)")
	}
	return messageID
}
