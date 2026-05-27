package query_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestQueryEngine_PostgresPortability exercises the three SQL shapes
// the external review flagged as failing on PostgreSQL:
//
//   - Aggregate uses `FROM ( … ) AS agg` (PG rejects unaliased derived
//     tables in FROM).
//   - GetGmailIDsByFilter avoids SELECT DISTINCT entirely (PG rejects
//     DISTINCT when ORDER BY references columns missing from SELECT).
//   - ListMessages sorts via expressions that textually match the
//     SELECT list (PG enforces this for SELECT DISTINCT).
//
// Runs against whichever backend testutil.NewTestStore picks up — on
// CI / dev machines that's SQLite; setting MSGVAULT_TEST_DB to a
// postgres:// DSN exercises the PG path that the bugs were specific
// to. The test never asserts on dialect-specific error text; a bug
// would surface as a generic Scan/Exec failure on PG.
func TestQueryEngine_PostgresPortability(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "pgcompat@example.com")
	require.NoError(err, "GetOrCreateSource")

	convID, err := st.EnsureConversation(src.ID, "thread-1", "Thread 1")
	require.NoError(err, "EnsureConversation")

	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err, "EnsureParticipant alice")
	bobID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err, "EnsureParticipant bob")

	labelID, err := st.EnsureLabel(src.ID, "Label_1", "Important", "user")
	require.NoError(err, "EnsureLabel")

	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := range 4 {
		mid, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: gmailSourceID(i),
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: base.Add(time.Duration(i) * time.Hour), Valid: true},
			Subject:         sql.NullString{String: subjectFor(i), Valid: true},
			Snippet:         sql.NullString{String: "snippet", Valid: true},
			SizeEstimate:    int64(1000 + i*250),
		})
		require.NoError(err, "UpsertMessage")
		require.NoError(st.ReplaceMessageRecipients(mid, "from", []int64{aliceID}, []string{"Alice"}),
			"ReplaceMessageRecipients from")
		require.NoError(st.ReplaceMessageRecipients(mid, "to", []int64{bobID}, []string{"Bob"}),
			"ReplaceMessageRecipients to")
		require.NoError(st.ReplaceMessageLabels(mid, []int64{labelID}),
			"ReplaceMessageLabels")
	}

	eng := query.NewEngine(st.DB(), st.IsPostgreSQL())
	ctx := context.Background()

	// (1) Aggregate — must not error with "syntax error near ')'" on PG.
	t.Run("aggregate_senders", func(t *testing.T) {
		rows, err := eng.Aggregate(ctx, query.ViewSenders, query.AggregateOptions{
			SortField:     query.SortByCount,
			SortDirection: query.SortDesc,
			Limit:         50,
		})
		requirepkg.NoError(t, err, "Aggregate")
		requirepkg.NotEmpty(t, rows, "Aggregate returned no rows; expected at least the Alice sender bucket")
	})

	// (2) GetGmailIDsByFilter — must not error from a SELECT DISTINCT +
	// ORDER BY collision on PG. Use a label filter to exercise the
	// previously-multiplying join (now an EXISTS subquery).
	t.Run("gmail_ids_by_filter_label", func(t *testing.T) {
		ids, err := eng.GetGmailIDsByFilter(ctx, query.MessageFilter{
			SourceID: &src.ID,
			Label:    "Important",
			Sorting: query.MessageSorting{
				Field:     query.MessageSortByDate,
				Direction: query.SortDesc,
			},
		})
		requirepkg.NoError(t, err, "GetGmailIDsByFilter")
		assertpkg.Len(t, ids, 4, "label join must not multiply")
		// Confirm no duplicates after dropping DISTINCT — every message
		// row should appear exactly once because the label filter is an
		// EXISTS subquery, not a 1:N JOIN.
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			_, dup := seen[id]
			assertpkg.False(t, dup, "duplicate id %q in result; EXISTS conversion broken", id)
			seen[id] = struct{}{}
		}
	})

	// (3) ListMessages sorted by size and subject — both previously
	// bound raw column references in ORDER BY that did not match the
	// COALESCE-wrapped expressions in the SELECT list. PG rejects that
	// combination under SELECT DISTINCT.
	for _, sort := range []struct {
		name  string
		field query.MessageSortField
	}{
		{"sort_by_size", query.MessageSortBySize},
		{"sort_by_subject", query.MessageSortBySubject},
		{"sort_by_date", query.MessageSortByDate},
	} {
		t.Run("list_messages_"+sort.name, func(t *testing.T) {
			msgs, err := eng.ListMessages(ctx, query.MessageFilter{
				SourceID: &src.ID,
				Sorting: query.MessageSorting{
					Field:     sort.field,
					Direction: query.SortDesc,
				},
				Pagination: query.Pagination{Limit: 50},
			})
			requirepkg.NoError(t, err, "ListMessages %s", sort.name)
			assertpkg.Len(t, msgs, 4, "ListMessages %s", sort.name)
		})
	}
}

// TestQueryEngine_CaseInsensitiveSearch_Subject verifies that
// `subject:` terms passed through query.Engine.Search match
// case-insensitively on both SQLite and PostgreSQL. SQLite's LIKE is
// ASCII-case-insensitive by default; PostgreSQL's LIKE is
// case-sensitive, so an unwrapped `m.subject LIKE ?` would mis-miss
// rows that the equivalent store API path (which lowercases) returns.
// Bare-LIKE divergence was H3 in the codex review.
func TestQueryEngine_CaseInsensitiveSearch_Subject(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "case-search@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "case-thread", "case thread")
	require.NoError(err, "EnsureConversation")
	mid, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "case-msg-1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		Subject:         sql.NullString{String: "Quarterly Invoice", Valid: true},
		Snippet:         sql.NullString{String: "see attached", Valid: true},
		SizeEstimate:    1024,
	})
	require.NoError(err, "UpsertMessage")
	_ = mid

	eng := query.NewEngine(st.DB(), st.IsPostgreSQL())
	ctx := context.Background()

	for _, term := range []string{"invoice", "INVOICE", "Invoice"} {
		got, err := eng.Search(ctx,
			&search.Query{SubjectTerms: []string{term}}, 50, 0)
		require.NoError(err, "Search subject=%q", term)
		assertpkg.Len(t, got, 1, "subject:%q against stored subject %q", term, "Quarterly Invoice")
	}
}

func gmailSourceID(i int) string {
	return "gmail-msg-" + string(rune('a'+i))
}

func subjectFor(i int) string {
	switch i {
	case 0:
		return "alpha subject"
	case 1:
		return "beta subject"
	case 2:
		return "gamma subject"
	default:
		return "delta subject"
	}
}
