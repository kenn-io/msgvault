package store_test

import (
	"database/sql"
	"testing"

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
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("gmail", "rank@example.com")
	if err != nil {
		t.Fatalf("GetOrCreateSource: %v", err)
	}
	convID, err := st.EnsureConversation(src.ID, "rank-thread", "Rank Thread")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

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
		if err != nil {
			t.Fatalf("UpsertMessage(%q): %v", srcMsgID, err)
		}
		return id
	}

	subjID := mkMsg("rank-subj", "zappa filler filler filler", "alpha beta gamma")
	fromID := mkMsg("rank-from", "alpha beta gamma filler", "alpha beta gamma")
	bodyID := mkMsg("rank-body", "alpha beta gamma filler", "zappa filler filler")

	// Push FTS docs explicitly so the test does not depend on the full
	// sync pipeline; FTSUpsert maps each field to the column the dialect
	// then weights.
	if err := st.UpsertFTS(subjID, "zappa filler filler filler", "alpha beta gamma", "noreply@example.com", "", ""); err != nil {
		t.Fatalf("UpsertFTS subj: %v", err)
	}
	// "zappa noreply@example.com" — keep zappa as its own whitespace-
	// delimited token so PG's text parser (which treats `user@host.tld`
	// as one email token) still matches it; SQLite's unicode61 tokenizer
	// splits on `@` and would match either way.
	if err := st.UpsertFTS(fromID, "alpha beta gamma filler", "alpha beta gamma", "zappa noreply@example.com", "", ""); err != nil {
		t.Fatalf("UpsertFTS from: %v", err)
	}
	if err := st.UpsertFTS(bodyID, "alpha beta gamma filler", "zappa filler filler", "noreply@example.com", "", ""); err != nil {
		t.Fatalf("UpsertFTS body: %v", err)
	}

	results, total, err := st.SearchMessages("zappa", 0, 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3 (subj/from/body all match)", total)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}

	gotOrder := []int64{results[0].ID, results[1].ID, results[2].ID}
	wantOrder := []int64{subjID, fromID, bodyID}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Errorf("rank position %d: got message %d, want %d (full order got=%v want=%v: subj=%d from=%d body=%d)",
				i, gotOrder[i], wantOrder[i], gotOrder, wantOrder, subjID, fromID, bodyID)
		}
	}
}
