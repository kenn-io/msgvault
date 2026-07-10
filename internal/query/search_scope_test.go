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

func TestSearchFast_MetadataUnicodeCaseFold(t *testing.T) {
	f := storetest.New(t)
	ctx := context.Background()

	subjectID := createSearchScopeMessage(t, f, "unicode-metadata-subject",
		"École newsletter", "ordinary preview", "ordinary body", 0, 0)
	snippetID := createSearchScopeMessage(t, f, "unicode-metadata-snippet",
		"ordinary subject", "École preview", "ordinary body", 0, 0)
	senderID := f.EnsureParticipant("unicode@example.com", "Élodie Example", "example.com")
	senderMessageID := createSearchScopeMessage(t, f, "unicode-metadata-sender",
		"ordinary subject", "ordinary preview", "ordinary body", senderID, 0)

	engine := query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL())
	tests := []struct {
		name    string
		term    string
		wantIDs []int64
	}{
		{name: "subject and snippet", term: "école", wantIDs: []int64{subjectID, snippetID}},
		{name: "participant display name", term: "élodie", wantIDs: []int64{senderMessageID}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			q := &search.Query{TextTerms: []string{tc.term}}
			var gotIDs []int64
			for offset := range len(tc.wantIDs) {
				page, err := engine.SearchFast(ctx, q, query.MessageFilter{}, 1, offset)
				require.NoError(err, "SearchFast page %d", offset)
				require.Len(page, 1, "SearchFast page %d", offset)
				gotIDs = append(gotIDs, page[0].ID)
			}
			assert.ElementsMatch(tc.wantIDs, gotIDs, "paginated message IDs")

			count, err := engine.SearchFastCount(ctx, q, query.MessageFilter{})
			require.NoError(err, "SearchFastCount")
			assert.Equal(int64(len(tc.wantIDs)), count, "total count")

			result, err := engine.SearchFastWithStats(ctx, q, tc.term,
				query.MessageFilter{}, query.ViewSenders, 1, 0)
			require.NoError(err, "SearchFastWithStats")
			require.NotNil(result.Stats, "metadata stats")
			assert.Equal(count, result.TotalCount, "result total")
			assert.Equal(count, result.Stats.MessageCount, "stats total")
		})
	}
}

func TestSearchFast_StructuredMetadataUnicodeCaseFold(t *testing.T) {
	f := storetest.New(t)
	ctx := context.Background()

	subjectID := createSearchScopeMessage(t, f, "unicode-structured-subject",
		"École newsletter", "ordinary preview", "ordinary body", 0, 0)
	labelMessageID := createSearchScopeMessage(t, f, "unicode-structured-label",
		"ordinary subject", "ordinary preview", "ordinary body", 0, 0)
	labels := f.EnsureLabels(map[string]string{"unicode-label": "Étiquette"}, "user")
	require.NoError(t, f.Store.ReplaceMessageLabels(labelMessageID, []int64{labels["unicode-label"]}),
		"ReplaceMessageLabels")

	engine := query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL())
	tests := []struct {
		name     string
		query    *search.Query
		queryStr string
		groupBy  query.ViewType
		wantID   int64
	}{
		{
			name:     "subject",
			query:    &search.Query{SubjectTerms: []string{"école"}},
			queryStr: "subject:école",
			groupBy:  query.ViewSenders,
			wantID:   subjectID,
		},
		{
			name:     "label",
			query:    &search.Query{Labels: []string{"étiquette"}},
			queryStr: "label:étiquette",
			groupBy:  query.ViewLabels,
			wantID:   labelMessageID,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			messages, err := engine.SearchFast(ctx, tc.query, query.MessageFilter{}, 10, 0)
			require.NoError(err, "SearchFast")
			require.Len(messages, 1, "structured metadata hit")
			assert.Equal(tc.wantID, messages[0].ID, "structured metadata hit ID")

			count, err := engine.SearchFastCount(ctx, tc.query, query.MessageFilter{})
			require.NoError(err, "SearchFastCount")
			assert.Equal(int64(1), count, "structured metadata count")

			result, err := engine.SearchFastWithStats(ctx, tc.query, tc.queryStr,
				query.MessageFilter{}, tc.groupBy, 10, 0)
			require.NoError(err, "SearchFastWithStats")
			require.NotNil(result.Stats, "structured metadata stats")
			assert.Equal(int64(1), result.TotalCount, "structured result total")
			assert.Equal(int64(1), result.Stats.MessageCount, "structured stats total")

			if tc.groupBy == query.ViewLabels {
				opts := query.DefaultAggregateOptions()
				opts.SearchQuery = tc.queryStr
				rows, err := engine.Aggregate(ctx, query.ViewLabels, opts)
				require.NoError(err, "Aggregate(ViewLabels)")
				require.Len(rows, 1, "Unicode label aggregate")
				assert.Equal("Étiquette", rows[0].Key, "Unicode label aggregate key")
			}
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

	engine := query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL())
	bodySearcher, ok := engine.(query.MessageBodySearcher)
	require.True(ok, "production query engine must expose exact body search")

	messages, err := bodySearcher.SearchMessageBodies(ctx, &search.Query{TextTerms: []string{"scopeword"}}, 50, 0)
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

	engine := query.NewEngine(f.Store.DB(), true)
	bodySearcher, ok := engine.(query.MessageBodySearcher)
	require.True(ok, "production PostgreSQL engine must expose exact body search")
	_, err = bodySearcher.SearchMessageBodies(context.Background(),
		&search.Query{TextTerms: []string{"stalecheck"}}, 50, 0)
	require.Error(err, "stale body search must fail closed")
	require.ErrorIs(err, query.ErrMessageBodySearchIndexStale)
	assert.Contains(err.Error(), "rebuild-fts")
	assert.Contains(err.Error(), "backfill")
}

func TestSearchMessageBodies_PhraseGrouping(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := context.Background()

	adjacentID := createSearchScopeMessage(t, f, "body-phrase-adjacent",
		"ordinary subject", "ordinary preview", "alpha beta marker", 0, 0)
	createSearchScopeMessage(t, f, "body-phrase-separated",
		"ordinary subject", "ordinary preview", "alpha between beta", 0, 0)
	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	engine := query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL())
	bodySearcher, ok := engine.(query.MessageBodySearcher)
	require.True(ok, "production query engine must expose exact body search")
	messages, err := bodySearcher.SearchMessageBodies(ctx,
		&search.Query{TextTerms: []string{"alpha beta"}}, 50, 0)
	require.NoError(err, "SearchMessageBodies")
	require.Len(messages, 1, "phrase body hits")
	assert.Equal(adjacentID, messages[0].ID, "adjacent phrase hit")
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
