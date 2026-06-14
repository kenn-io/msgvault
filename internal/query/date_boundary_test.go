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

// TestDateBoundary_StoreAPIMatchesEngine pins the C8 fix: the store-API
// date filters (internal/store/api.go) must bind the raw time.Time rather
// than an RFC3339 ('T'-separated) string, so the after:/before: day boundary
// lands in exactly the same place as the query engine, on both backends.
//
// The hazard the fix removes:
//   - On PG TIMESTAMPTZ, a formatted offset-less string is parsed in the
//     session TimeZone, not UTC, shifting the boundary under a non-UTC session.
//   - Against SQLite's space-separated stored timestamps, the 'T' byte (0x54)
//     sorts after a space (0x20), shifting the boundary the other way.
//
// A message stored at exactly midnight UTC is the worst case: a one-byte
// boundary shift flips its inclusion. The test asserts the store API agrees
// with the engine for every after:/before: bound around that midnight — no
// off-by-one-day divergence between the two paths or the two backends.
//
// Runs against whichever backend testutil.NewTestStore selects; setting
// MSGVAULT_TEST_DB to a postgres:// DSN exercises the PG path too.
func TestDateBoundary_StoreAPIMatchesEngine(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	src, err := st.GetOrCreateSource("gmail", "boundary@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-1", "Thread 1")
	require.NoError(err, "EnsureConversation")

	// Message lands at exactly midnight UTC — the worst case for a boundary
	// shift, since after:>= and before:< both pivot on this instant.
	midnight := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	nextDay := midnight.AddDate(0, 0, 1)
	const gmailID = "boundary-msg-1"

	_, err = st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: gmailID,
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: midnight, Valid: true},
		Subject:         sql.NullString{String: "boundary", Valid: true},
		SizeEstimate:    1000,
	})
	require.NoError(err, "UpsertMessage")

	eng := query.NewEngine(st.DB(), st.IsPostgreSQL())

	// storeHas runs the store-API search with the given after/before bounds
	// and reports whether the midnight message is included.
	storeHas := func(t *testing.T, after, before *time.Time) bool {
		t.Helper()
		msgs, total, err := st.SearchMessagesQuery(
			&search.Query{AfterDate: after, BeforeDate: before}, 0, 50,
		)
		requirepkg.NoError(t, err, "store SearchMessagesQuery")
		requirepkg.Equal(t, int64(len(msgs)), total, "store total vs page mismatch")
		return total == 1
	}

	// engineHas runs the query engine's after:/before: path (SearchFast,
	// which binds *q.AfterDate/*q.BeforeDate at the [cr2-9] site the store
	// fix now mirrors) with the same bounds and reports whether the midnight
	// message is included. SearchFast filters on m.sent_at directly; the
	// message sets only sent_at, so the store API's COALESCE collapses to the
	// same column — an apples-to-apples boundary comparison.
	engineHas := func(t *testing.T, after, before *time.Time) bool {
		t.Helper()
		msgs, err := eng.SearchFast(ctx, &search.Query{}, query.MessageFilter{
			SourceID: &src.ID,
			After:    after,
			Before:   before,
		}, 50, 0)
		requirepkg.NoError(t, err, "engine SearchFast")
		for _, m := range msgs {
			if m.SourceMessageID == gmailID {
				return true
			}
		}
		return false
	}

	cases := []struct {
		name        string
		after       *time.Time
		before      *time.Time
		wantInclude bool
	}{
		// after: is inclusive (>=). A bound exactly at midnight must INCLUDE
		// the message; a 'T'-string bind on SQLite would push it just past
		// the stored space-separated value and drop it.
		{"after_at_midnight_includes", &midnight, nil, true},
		// after: the next day must EXCLUDE the midnight message.
		{"after_next_day_excludes", &nextDay, nil, false},
		// before: is exclusive (<). A bound exactly at midnight must EXCLUDE
		// the message stored at that same instant.
		{"before_at_midnight_excludes", nil, &midnight, false},
		// before: the next day must INCLUDE the midnight message.
		{"before_next_day_includes", nil, &nextDay, true},
		// Half-open window [midnight, nextDay) must INCLUDE the message.
		{"window_includes", &midnight, &nextDay, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStore := storeHas(t, tc.after, tc.before)
			gotEngine := engineHas(t, tc.after, tc.before)

			// Cross-path agreement is the core regression guard: the store
			// API and the engine must never disagree at the boundary.
			assertpkg.Equal(t, gotEngine, gotStore,
				"store-API and query-engine boundary divergence for %s", tc.name)
			// And both must match the timezone-stable expectation.
			assertpkg.Equal(t, tc.wantInclude, gotStore,
				"store-API inclusion wrong for %s", tc.name)
			assertpkg.Equal(t, tc.wantInclude, gotEngine,
				"query-engine inclusion wrong for %s", tc.name)
		})
	}
}
