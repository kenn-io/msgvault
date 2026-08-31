package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetDuplicateGroupMessages_UsesRFC822Index verifies InitSchema creates
// idx_messages_rfc822_message_id and that the dedup per-group lookup query
// plans as an index search, not a full table scan. Locks in the fix for
// kenn-io/msgvault#510: 22,025 unindexed lookups (~190ms each) burned the
// CLI's 30-minute plan-request timeout before content-hash comparison
// started.
func TestGetDuplicateGroupMessages_UsesRFC822Index(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	s, err := OpenForTest(filepath.Join(dir, "dedup_index.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	var idxCount int
	require.NoError(s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_messages_rfc822_message_id'`,
	).Scan(&idxCount))
	assert.Equal(1, idxCount, "idx_messages_rfc822_message_id should be created by InitSchema")

	plan := explainPlan(t, s,
		`SELECT id FROM messages WHERE rfc822_message_id = ?`, "some-id")
	assert.Contains(plan, "idx_messages_rfc822_message_id",
		"lookup by rfc822_message_id should use the index, not a full scan:\n%s", plan)
	assert.NotContains(plan, "SCAN messages",
		"lookup should not do a full table scan:\n%s", plan)

	batchPlan := explainPlan(t, s,
		`SELECT id FROM messages WHERE rfc822_message_id IN (?,?)`, "a", "b")
	assert.Contains(batchPlan, "idx_messages_rfc822_message_id",
		"batched IN lookup should also use the index:\n%s", batchPlan)
	assert.NotContains(batchPlan, "SCAN messages",
		"batched lookup should not do a full table scan:\n%s", batchPlan)
}

// seedDuplicateDiscoveryRows inserts messages across three sources: source 1
// has a bracketed/bare pair, a malformed pair that must not be unwrapped, and
// two singletons; source 2 turns one singleton into a cross-source duplicate;
// source 3 is outside the production-shaped scopes and would turn the other
// singleton into a duplicate if source filtering leaked.
func seedDuplicateDiscoveryRows(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.db.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES
			(1, 'gmail', 'plan-index-1@example.test'),
			(2, 'imap', 'plan-index-2@example.test'),
			(3, 'mbox', 'plan-index-3@example.test');
		INSERT INTO conversations (id, source_id, conversation_type) VALUES
			(1, 1, 'email_thread'),
			(2, 2, 'email_thread'),
			(3, 3, 'email_thread');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, rfc822_message_id) VALUES
			(1, 1, 1, 'bracketed', 'email', '<canonical@example.test>'),
			(2, 1, 1, 'bare', 'email', 'canonical@example.test'),
			(3, 1, 1, 'malformed-first', 'email', '<<malformed@example.test>>'),
			(4, 1, 1, 'malformed-second', 'email', '<<malformed@example.test>>'),
			(5, 1, 1, 'cross-source-first', 'email', '<cross-source@example.test>'),
			(6, 2, 2, 'cross-source-second', 'email', 'cross-source@example.test'),
			(7, 1, 1, 'scoped-singleton', 'email', 'scope-leak@example.test'),
			(8, 3, 3, 'out-of-scope-copy', 'email', 'scope-leak@example.test'),
			(9, 3, 3, 'out-of-scope-first', 'email', 'outside@example.test'),
			(10, 3, 3, 'out-of-scope-second', 'email', 'outside@example.test');
	`)
	require.NoError(t, err, "seed duplicate discovery rows")
}

// TestFindDuplicatesByRFC822ID_UsesCanonicalExpressionIndex verifies the
// duplicate-discovery GROUP BY over the canonical Message-ID expression is
// served by the composite canonical/source index instead of SQLite choosing
// idx_messages_source and building a temp B-tree. Both the ordinary account
// shape (one source) and collection shape (multiple sources) run the exact
// production query, so expression drift or removal of the production index
// selection would make these plans regress. The test also covers fresh and
// upgraded schema creation and verifies scope and malformed-ID semantics.
func TestFindDuplicatesByRFC822ID_UsesCanonicalExpressionIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	s, err := OpenForTest(filepath.Join(dir, "dedup_canonical_index.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	indexPresent := func() bool {
		var idxCount int
		require.NoError(s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`,
			rfc822CanonicalIndexName,
		).Scan(&idxCount))
		return idxCount == 1
	}

	// Fresh schemas carry the index; existing databases that ran InitSchema
	// before the index existed pick it up on the next InitSchema.
	require.True(indexPresent(), "fresh schema must create "+rfc822CanonicalIndexName)
	_, err = s.db.Exec(`DROP INDEX ` + rfc822CanonicalIndexName)
	require.NoError(err, "drop the index to simulate a pre-index database")
	require.False(indexPresent(), "index must be gone after the drop")
	require.NoError(s.InitSchema(), "re-init must upgrade the existing database")
	require.True(indexPresent(), "upgraded database must recreate "+rfc822CanonicalIndexName)

	seedDuplicateDiscoveryRows(t, s)

	for _, tc := range []struct {
		name      string
		sourceIDs []int64
		want      map[string]int
	}{
		{
			name:      "single account source",
			sourceIDs: []int64{1},
			want: map[string]int{
				"canonical@example.test":     2,
				"<<malformed@example.test>>": 2,
			},
		},
		{
			name:      "multi-source collection",
			sourceIDs: []int64{1, 2},
			want: map[string]int{
				"canonical@example.test":     2,
				"cross-source@example.test":  2,
				"<<malformed@example.test>>": 2,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query, args := s.findDuplicatesByRFC822IDQuery(tc.sourceIDs)
			plan := explainPlan(t, s, query, args...)
			assert.Contains(plan, rfc822CanonicalIndexName,
				"scoped duplicate discovery should scan the canonical/source index:\n%s", plan)
			assert.NotContains(plan, "USE TEMP B-TREE FOR GROUP BY",
				"grouping should stream from canonical index order, not sort:\n%s", plan)
			assert.NotContains(plan, "idx_messages_source (source_id=?)",
				"the source-only index cannot serve canonical grouping:\n%s", plan)

			groups, err := s.FindDuplicatesByRFC822ID(tc.sourceIDs...)
			require.NoError(err, "FindDuplicatesByRFC822ID")
			byKey := make(map[string]int, len(groups))
			for _, g := range groups {
				byKey[g.RFC822MessageID] = g.Count
			}
			assert.Equal(tc.want, byKey,
				"scoped discovery results with the expression index in place")
		})
	}
}
