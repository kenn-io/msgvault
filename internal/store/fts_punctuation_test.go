package store_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"example@both"}}, 10)
	require.NoError(t, err)
	require.Empty(t, ids)
}

func TestPostgresFTSSaturatedPhrases(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL indexing")
	}
	for i, prefix := range []string{strings.Repeat("filler ", 17000), strings.Repeat("alice ", 255), strings.Repeat("alice ", 300)} {
		id := f.NewMessage().WithSourceMessageID(strconv.Itoa(i)).Create(t, f.Store)
		require.NoError(f.Store.UpsertFTS(id, "", prefix+"alice@example.com", "", "", ""))
		ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice@exam"}}, 10)
		require.NoError(err)
		assert.Contains(ids, id)
		ids, _, err = f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"example@alice"}}, 10)
		require.NoError(err)
		assert.NotContains(ids, id)
	}
	separated := f.NewMessage().WithSourceMessageID("separated").Create(t, f.Store)
	crossField := f.NewMessage().WithSourceMessageID("cross-field-saturated").Create(t, f.Store)
	require.NoError(f.Store.UpsertFTS(separated, "", strings.Repeat("filler ", 17000)+"alice middle example", "", "", ""))
	require.NoError(f.Store.UpsertFTS(crossField, "alice", strings.Repeat("filler ", 17000)+"example", "", "", ""))
	ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice@example"}}, 10)
	require.NoError(err)
	assert.NotContains(ids, separated)
	assert.NotContains(ids, crossField)
	unicodeID := f.NewMessage().WithSourceMessageID("unicode-saturated").Create(t, f.Store)
	require.NoError(f.Store.UpsertFTS(unicodeID, "", strings.Repeat("filler ", 17000)+"CAFÉ@東京.example e\u0301xample", "", "", ""))
	for _, term := range []string{"café@東京.exam", "e@xam"} {
		ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{term}}, 10)
		require.NoError(err)
		assert.Contains(ids, unicodeID)
	}
}

func TestPostgresFTSPhraseDoesNotCrossFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL weighted search vector")
	}
	crossField := f.NewMessage().WithSourceMessageID("cross-field").Create(t, f.Store)
	crossRecipients := f.NewMessage().WithSourceMessageID("cross-recipients").Create(t, f.Store)
	complete := f.NewMessage().WithSourceMessageID("complete-address").Create(t, f.Store)
	require.NoError(f.Store.UpsertFTS(crossField, "alice", "", "example.com", "", ""))
	require.NoError(f.Store.UpsertFTS(crossRecipients, "", "", "", "alice", "example.com"))
	require.NoError(f.Store.UpsertFTS(complete, "", "alice@example.com", "", "", ""))

	ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice@example.com"}}, 10)
	require.NoError(err)
	assert.Contains(ids, complete)
	assert.NotContains(ids, crossField)
	assert.NotContains(ids, crossRecipients)

	ids, _, err = f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice", "example"}}, 10)
	require.NoError(err)
	assert.ElementsMatch([]int64{crossField, crossRecipients, complete}, ids)
}
