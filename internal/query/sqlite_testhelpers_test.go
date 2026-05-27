package query

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

// testEnv encapsulates the DB, Engine, and Context setup for tests.
type testEnv struct {
	*dbtest.TestDB
	Engine *SQLiteEngine
	Ctx    context.Context
}

// newTestEnv creates a test environment with an in-memory SQLite database and test data.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	tdb.SeedStandardDataSet()
	return &testEnv{
		TestDB: tdb,
		Engine: NewSQLiteEngine(tdb.DB),
		Ctx:    context.Background(),
	}
}

// MustListMessages calls ListMessages and fails the test on error.
func (e *testEnv) MustListMessages(filter MessageFilter) []MessageSummary {
	e.T.Helper()
	messages, err := e.Engine.ListMessages(e.Ctx, filter)
	require.NoError(e.T, err, "ListMessages")
	return messages
}

// MustSearch calls Search and fails the test on error.
func (e *testEnv) MustSearch(q *search.Query, limit, offset int) []MessageSummary {
	e.T.Helper()
	results, err := e.Engine.Search(e.Ctx, q, limit, offset)
	require.NoError(e.T, err, "Search")
	return results
}

// MustGetTotalStats calls GetTotalStats and fails the test on error.
func (e *testEnv) MustGetTotalStats(opts StatsOptions) *TotalStats {
	e.T.Helper()
	stats, err := e.Engine.GetTotalStats(e.Ctx, opts)
	require.NoError(e.T, err, "GetTotalStats")
	return stats
}

// EnableFTS creates the FTS5 table, populates it, and re-creates the engine
// to clear cached FTS state.
func (e *testEnv) EnableFTS() {
	e.T.Helper()
	e.TestDB.EnableFTS()
	// Re-create engine to clear cached FTS state.
	e.Engine = NewSQLiteEngine(e.DB)
}

// aggExpectation describes an expected key/count pair in aggregate results.
type aggExpectation struct {
	Key   string
	Count int64
}

// aggRowMap builds a map from key to count, failing on duplicate keys.
func aggRowMap(t *testing.T, rows []AggregateRow) map[string]int64 {
	t.Helper()
	m := make(map[string]int64, len(rows))
	for _, r := range rows {
		_, exists := m[r.Key]
		assert.False(t, exists, "duplicate key %q in results", r.Key)
		m[r.Key] = r.Count
	}
	return m
}

// assertRowsContain verifies that a subset of expected key/count pairs exist
// in the aggregate rows (order-independent). Also checks for duplicate keys.
func assertRowsContain(t *testing.T, rows []AggregateRow, want []aggExpectation) {
	t.Helper()
	m := aggRowMap(t, rows)
	for _, w := range want {
		got, ok := m[w.Key]
		if !assert.True(t, ok, "key %q not found in results", w.Key) {
			continue
		}
		assert.Equal(t, w.Count, got, "key %q count", w.Key)
	}
}

// assertRow finds a single key in the aggregate rows and asserts its count.
func assertRow(t *testing.T, rows []AggregateRow, key string, count int64) {
	t.Helper()
	m := aggRowMap(t, rows)
	got, ok := m[key]
	if !assert.True(t, ok, "key %q not found in results", key) {
		return
	}
	assert.Equal(t, count, got, "key %q count", key)
}

// assertAggRows verifies that aggregate rows contain the expected key/count pairs
// in the exact order given. This ensures both correctness and default sort behavior.
// Also checks for duplicate keys.
func assertAggRows(t *testing.T, rows []AggregateRow, want []aggExpectation) {
	t.Helper()
	aggRowMap(t, rows) // checks for duplicates
	assert.Len(t, rows, len(want), "aggregate rows length")
	for i := range want {
		if i >= len(rows) {
			break
		}
		assert.Equal(t, want[i].Key, rows[i].Key, "row[%d] key", i)
		assert.Equal(t, want[i].Count, rows[i].Count, "row[%d] (key %q) count", i, rows[i].Key)
	}
}

// assertSearchCount runs a search and verifies the number of results.
func assertSearchCount(t *testing.T, env *testEnv, q *search.Query, want int) []MessageSummary {
	t.Helper()
	results := env.MustSearch(q, 100, 0)
	assert.Len(t, results, want, "Search(%+v)", q)
	return results
}

// assertAllResults verifies that every result satisfies the given predicate.
func assertAllResults(t *testing.T, results []MessageSummary, desc string, pred func(MessageSummary) bool) {
	t.Helper()
	for _, r := range results {
		assert.True(t, pred(r), "result id=%d did not satisfy %s", r.ID, desc)
	}
}

// assertAnyResult verifies that at least one result satisfies the given predicate.
func assertAnyResult(t *testing.T, results []MessageSummary, desc string, pred func(MessageSummary) bool) {
	t.Helper()
	if slices.ContainsFunc(results, pred) {
		return
	}
	assert.Fail(t, "no result satisfied "+desc)
}

// newTestEnvWithEmptyBuckets creates a test DB with messages that have
// empty senders, recipients, domains, and labels for testing MatchEmpty* filters.
func newTestEnvWithEmptyBuckets(t *testing.T) *testEnv {
	t.Helper()

	env := newTestEnv(t)

	// Resolve participant IDs dynamically to avoid coupling to seed order.
	aliceID := env.MustLookupParticipant("alice@example.com")
	bobID := env.MustLookupParticipant("bob@company.org")

	// Participant with empty domain
	emptyDomainID := env.AddParticipant(dbtest.ParticipantOpts{
		Email:       new("nodomain@"),
		DisplayName: new("No Domain User"),
		Domain:      "",
	})

	// Message with no sender (msg6)
	env.AddMessage(dbtest.MessageOpts{
		Subject: "No Sender",
		SentAt:  "2024-04-01 10:00:00",
	})

	// Message with no recipients (msg7)
	env.AddMessage(dbtest.MessageOpts{
		Subject: "No Recipients",
		SentAt:  "2024-04-02 10:00:00",
		FromID:  aliceID,
	})

	// Message with empty domain sender (msg8)
	env.AddMessage(dbtest.MessageOpts{
		Subject: "Empty Domain",
		SentAt:  "2024-04-03 10:00:00",
		FromID:  emptyDomainID,
		ToIDs:   []int64{aliceID},
	})

	// Message with no labels (msg9)
	env.AddMessage(dbtest.MessageOpts{
		Subject: "No Labels",
		SentAt:  "2024-04-04 10:00:00",
		FromID:  aliceID,
		ToIDs:   []int64{bobID},
	})

	return env
}
