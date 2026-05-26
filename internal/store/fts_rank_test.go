package store_test

import (
	"database/sql"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}

// TestFTSRankWeightsAcrossBackends verifies that subject matches outrank
// sender matches, which outrank body-only matches. The same ordering must
// hold on SQLite (bm25 with column weights) and PostgreSQL (ts_rank over
// setweight'd tsvector). Each message is seeded with the search token
// "zappa" in exactly one FTS column so the rank attribution is unambiguous.
func TestFTSRankWeightsAcrossBackends(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("gmail", "rank@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "rank-thread", "Rank Thread")
	require.NoError(err, "EnsureConversation")

	// Seed three rows with the rare token in different columns. Subject
	// and body get filler distinct text so document length is comparable;
	// bm25 docs of vastly different lengths skew the score.
	mkMsg := func(srcMsgID, subject, snippet string) int64 {
		id, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: srcMsgID,
			MessageType:     "email",
			Subject:         nullString(subject),
			Snippet:         nullString(snippet),
			SizeEstimate:    100,
		})
		require.NoError(err, "UpsertMessage(%q)", srcMsgID)
		return id
	}

	subjID := mkMsg("rank-subj", "zappa filler filler filler", "alpha beta gamma")
	fromID := mkMsg("rank-from", "alpha beta gamma filler", "alpha beta gamma")
	bodyID := mkMsg("rank-body", "alpha beta gamma filler", "zappa filler filler")

	// Push FTS docs explicitly so the test does not depend on the full
	// sync pipeline; FTSUpsert maps each field to the column the dialect
	// then weights.
	require.NoError(st.UpsertFTS(subjID, "zappa filler filler filler", "alpha beta gamma", "noreply@example.com", "", ""), "UpsertFTS subj")
	// "zappa noreply@example.com" — keep zappa as its own whitespace-
	// delimited token so PG's text parser (which treats `user@host.tld`
	// as one email token) still matches it; SQLite's unicode61 tokenizer
	// splits on `@` and would match either way.
	require.NoError(st.UpsertFTS(fromID, "alpha beta gamma filler", "alpha beta gamma", "zappa noreply@example.com", "", ""), "UpsertFTS from")
	require.NoError(st.UpsertFTS(bodyID, "alpha beta gamma filler", "zappa filler filler", "noreply@example.com", "", ""), "UpsertFTS body")

	results, total, err := st.SearchMessages("zappa", 0, 10)
	require.NoError(err, "SearchMessages")
	require.Equal(int64(3), total, "total (subj/from/body all match)")
	require.Len(results, 3)

	gotOrder := []int64{results[0].ID, results[1].ID, results[2].ID}
	wantOrder := []int64{subjID, fromID, bodyID}
	for i := range wantOrder {
		assertpkg.Equal(t, wantOrder[i], gotOrder[i],
			"rank position %d (full order got=%v want=%v: subj=%d from=%d body=%d)",
			i, gotOrder, wantOrder, subjID, fromID, bodyID)
	}
}
