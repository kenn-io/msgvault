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
