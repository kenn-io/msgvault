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
	for term, want := range map[string][]int64{"notes": {id}, "both@example.com": {id}, "both@exam": {id}, "changelog.example.net": {id}, "1.2.0": {id}, "+15550100777": {id}, "15550100777": {id}, "example@both": nil} {
		ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{term}}, 10)
		require.NoError(t, err, term)
		assert.ElementsMatch(t, want, ids, term)
	}
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
	plainCrossField := f.NewMessage().WithSourceMessageID("cross-field").Create(t, f.Store)
	require.NoError(f.Store.UpsertFTS(separated, "", strings.Repeat("filler ", 17000)+"alice middle example", "", "", ""))
	require.NoError(f.Store.UpsertFTS(crossField, "alice", strings.Repeat("filler ", 17000)+"example", "", "", ""))
	require.NoError(f.Store.UpsertFTS(plainCrossField, "alice", "", "example.com", "", ""))
	ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice@example"}}, 10)
	require.NoError(err)
	assert.NotContains(ids, separated)
	assert.NotContains(ids, crossField)
	assert.NotContains(ids, plainCrossField)
	ids, _, err = f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice", "example"}}, 10)
	require.NoError(err)
	assert.Contains(ids, plainCrossField)
	unicodeID := f.NewMessage().WithSourceMessageID("unicode-saturated").Create(t, f.Store)
	require.NoError(f.Store.UpsertFTS(unicodeID, "", strings.Repeat("filler ", 17000)+"CAFÉ@東京.example e\u0301xample", "", "", ""))
	for _, term := range []string{"café@東京.exam", "e@xam"} {
		ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{term}}, 10)
		require.NoError(err)
		assert.Contains(ids, unicodeID)
	}
}
