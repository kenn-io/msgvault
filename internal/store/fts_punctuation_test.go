package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSearchFTSPunctuation(t *testing.T) {
	f := storetest.New(t)
	id := f.NewMessage().WithSourceMessageID("m1").WithSubject("Release 1.2.0 notes").Create(t, f.Store)
	require.NoError(t, f.Store.UpsertFTS(id, "Release 1.2.0 notes", "Write to both@example.com or see changelog.example.net.", "+15550100777", "", ""))
	for _, term := range []string{"notes", "both@example.com", "both@exam", "changelog.example.net", "1.2.0", "+15550100777", "15550100777"} {
		t.Run(term, func(t *testing.T) {
			ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{term}}, 10)
			require.NoError(t, err)
			require.Equal(t, []int64{id}, ids)
		})
	}
}

func TestPostgresFTSPunctuationBackfill(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL indexing")
	}
	id := f.NewMessage().WithSourceMessageID("punctuation").WithSubject("Release 1.2.0 notes").Create(t, f.Store)
	body := "Write to both@example.com or see changelog.example.net."
	require.NoError(t, f.Store.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	require.NoError(t, f.Store.UpsertFTS(id, "Release 1.2.0 notes", body, "", "", ""))
	searchFor := func(term string, want []int64) {
		t.Helper()
		ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{term}}, 10)
		require.NoError(t, err)
		require.Equal(t, want, ids, term)
	}
	for _, term := range []string{"both@example.com", "both@exam", "1.2.0"} {
		searchFor(term, []int64{id})
	}
	searchFor("example@both", []int64{})
	_, err := f.Store.DB().ExecContext(t.Context(), "UPDATE messages SET indexing_version = 2 WHERE id = $1", id)
	require.NoError(t, err)
	_, err = f.Store.BackfillFTS(nil)
	require.NoError(t, err)
	for _, term := range []string{"both@example.com", "both@exam", "1.2.0"} {
		searchFor(term, []int64{id})
	}
	searchFor("example@both", []int64{})
}

func TestPostgresFTSPhraseDoesNotCrossFields(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL weighted search vector")
	}
	crossField := f.NewMessage().WithSourceMessageID("cross-field").Create(t, f.Store)
	crossRecipients := f.NewMessage().WithSourceMessageID("cross-recipients").Create(t, f.Store)
	complete := f.NewMessage().WithSourceMessageID("complete-address").Create(t, f.Store)
	require.NoError(t, f.Store.UpsertMessageBody(complete, sql.NullString{String: "alice@example.com", Valid: true}, sql.NullString{}))
	require.NoError(t, f.Store.UpsertFTS(crossField, "alice", "", "example.com", "", ""))
	require.NoError(t, f.Store.UpsertFTS(crossRecipients, "", "", "", "alice", "example.com"))
	require.NoError(t, f.Store.UpsertFTS(complete, "", "alice@example.com", "", "", ""))

	ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice@example.com"}}, 10)
	require.NoError(t, err)
	assert.Contains(t, ids, complete)
	assert.NotContains(t, ids, crossField)
	assert.NotContains(t, ids, crossRecipients)

	ids, _, err = f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice", "example"}}, 10)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int64{crossField, crossRecipients, complete}, ids)

	engine := query.NewEngine(f.Store.DB(), true)
	bodySearcher, ok := engine.(query.MessageBodySearcher)
	require.True(t, ok)
	results, err := bodySearcher.SearchMessageBodies(t.Context(), &search.Query{TextTerms: []string{"alice@example.com"}}, 10, 0)
	require.NoError(t, err)
	require.Len(t, results.Messages, 1)
	assert.Equal(t, complete, results.Messages[0].ID)
}
