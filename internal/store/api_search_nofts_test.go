package store

import (
	"database/sql"
	"fmt"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
)

// TestSearchMessagesQueryImpl_NoFTS_TokenlessTerms guards the LIKE fallback
// path (ftsAvailable=false), which is reached at runtime when FTS errors or
// when the binary is built without the fts5 tag. A text term that reduces to no
// searchable tokens (empty string, punctuation-only) must yield zero rows via a
// FALSE predicate — never "LOWER(...) LIKE '%%'", which matches every message.
// This mirrors the FTS path's tokenless handling. It forces the no-FTS branch
// directly, so it runs regardless of the fts5 build tag.
func TestSearchMessagesQueryImpl_NoFTS_TokenlessTerms(t *testing.T) {
	require := requirepkg.New(t)
	st := openTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "noftstokenless@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-nofts", "Thread NoFTS")
	require.NoError(err, "EnsureConversation")

	for i, sub := range []string{"invoice attached", "project update"} {
		_, err := st.UpsertMessage(&Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("nofts-msg-%d", i),
			MessageType:     "email",
			Subject:         sql.NullString{String: sub, Valid: true},
			Snippet:         sql.NullString{String: "weekly snippet", Valid: true},
			SizeEstimate:    100,
		})
		require.NoError(err, "UpsertMessage %d", i)
	}

	// Baseline: a real term still matches via LIKE, proving the setup is wired.
	_, total, err := st.searchMessagesQueryImpl(
		&search.Query{TextTerms: []string{"invoice"}}, 0, 50, false)
	require.NoError(err, "baseline LIKE search")
	require.GreaterOrEqual(total, int64(1), "baseline LIKE term must match")

	cases := []struct {
		name  string
		terms []string
	}{
		{"empty_string", []string{""}},
		{"only_punctuation", []string{"!!!"}},
		{"only_dashes", []string{"---"}},
		{"mixed_all_empty", []string{"!!!", "---", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs, total, err := st.searchMessagesQueryImpl(
				&search.Query{TextTerms: tc.terms}, 0, 50, false)
			requirepkg.NoError(t, err, "searchMessagesQueryImpl(%v)", tc.terms)
			assertpkg.Equal(t, int64(0), total, "tokenless terms must match nothing on the LIKE path")
			assertpkg.Empty(t, msgs)
		})
	}
}
