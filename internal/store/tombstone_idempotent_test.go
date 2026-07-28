package store_test

import (
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// deletedSentinel is a fixed, far-past timestamp string parseable by both the
// SQLite DATETIME column and the PostgreSQL TIMESTAMPTZ column (same format
// baselineLM uses for last_modified in last_modified_test.go). Because a
// provider repair scan re-reports the same already-deleted IDs on every run,
// the five tombstone writers must leave an existing stamp alone; stamping a
// sentinel this far from "now" makes an unguarded rewrite trivially visible
// regardless of clock or driver timestamp resolution.
const deletedSentinel = "2000-01-01 00:00:00+00"

// setDeletedFromSourceAt stamps sourceMessageID's tombstone directly,
// bypassing the writers under test, so tests can seed an "already tombstoned"
// row with a value distinguishable from any real "now" the writers would
// produce. It returns the value as the DATABASE renders it back, which is what
// callers must compare against rather than deletedSentinel:
// PostgreSQL renders a TIMESTAMPTZ through the session's TimeZone setting, so
// a test asserting against the literal only holds under UTC. baselineLM in
// last_modified_test.go reads its sentinel back for the same reason.
func setDeletedFromSourceAt(t *testing.T, st *store.Store, sourceID int64, sourceMessageID string) string {
	t.Helper()
	_, err := st.DB().Exec(st.Rebind(`
		UPDATE messages SET deleted_from_source_at = ?
		WHERE source_id = ? AND source_message_id = ?
	`), deletedSentinel, sourceID, sourceMessageID)
	require.NoError(t, err, "set deleted_from_source_at sentinel")
	seeded := readDeletedFromSourceAt(t, st, sourceID, sourceMessageID)
	require.True(t, seeded.Valid, "sentinel must be readable after seeding")
	return seeded.String
}

// readDeletedFromSourceAt reads deleted_from_source_at as a comparable string
// on both backends (CAST to TEXT defeats go-sqlite3's DATETIME->time.Time
// coercion, matching the readLM helper's trick in last_modified_test.go).
func readDeletedFromSourceAt(t *testing.T, st *store.Store, sourceID int64, sourceMessageID string) sql.NullString {
	t.Helper()
	var s sql.NullString
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT CAST(deleted_from_source_at AS TEXT) FROM messages WHERE source_id = ? AND source_message_id = ?`),
		sourceID, sourceMessageID,
	).Scan(&s), "read deleted_from_source_at")
	return s
}

// tombstoneWriter adapts each of the five deleted_from_source_at writers to a
// common shape so the guard contract can be exercised identically across all
// of them. id is always the source_message_id (== Gmail ID for the two
// GmailID-keyed writers).
type tombstoneWriter func(t *testing.T, st *store.Store, sourceID int64, id string) error

func tombstoneWriters() map[string]tombstoneWriter {
	return map[string]tombstoneWriter{
		"MarkMessageDeleted": func(t *testing.T, st *store.Store, sourceID int64, id string) error {
			t.Helper()
			return st.MarkMessageDeleted(sourceID, id)
		},
		"MarkMessagesDeletedBatch": func(t *testing.T, st *store.Store, sourceID int64, id string) error {
			t.Helper()
			return st.MarkMessagesDeletedBatch(sourceID, []string{id})
		},
		"MarkMessagesDeletedFromReader": func(t *testing.T, st *store.Store, sourceID int64, id string) error {
			t.Helper()
			return st.MarkMessagesDeletedFromReader(sourceID, strings.NewReader(id+"\n"), 10)
		},
		"MarkMessageDeletedByGmailID": func(t *testing.T, st *store.Store, sourceID int64, id string) error {
			t.Helper()
			return st.MarkMessageDeletedByGmailID(false, id)
		},
		"MarkMessagesDeletedByGmailIDBatch": func(t *testing.T, st *store.Store, sourceID int64, id string) error {
			t.Helper()
			return st.MarkMessagesDeletedByGmailIDBatch([]string{id})
		},
	}
}

// sortedWriterNames returns tombstoneWriters' keys in a fixed order so
// subtests run deterministically.
func sortedWriterNames() []string {
	writers := tombstoneWriters()
	names := make([]string, 0, len(writers))
	for name := range writers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// seedTombstoneMessage creates a source, conversation, and one message with
// the given source_message_id, returning the source ID.
func seedTombstoneMessage(t *testing.T, st *store.Store, account, sourceMessageID string) int64 {
	t.Helper()
	source, err := st.GetOrCreateSource("gmail", account)
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(source.ID, "conv-"+sourceMessageID, "channel", "general")
	require.NoError(t, err, "EnsureConversationWithType")
	insertStoreTestMessage(t, st, source.ID, convID, sourceMessageID)
	return source.ID
}

// TestTombstoneWriters_PreserveExistingStamp is the core regression: a
// provider repair scan re-reports the same already-deleted IDs on every run,
// so re-marking an already-tombstoned message must NOT rewrite
// deleted_from_source_at. Before the fix this fails because every writer
// unconditionally overwrites the column with the current time.
func TestTombstoneWriters_PreserveExistingStamp(t *testing.T) {
	writers := tombstoneWriters()
	for _, name := range sortedWriterNames() {
		writer := writers[name]
		t.Run(name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			sourceID := seedTombstoneMessage(t, st, "preserve-"+name, "msg-"+name)
			seeded := setDeletedFromSourceAt(t, st, sourceID, "msg-"+name)

			require.NoError(t, writer(t, st, sourceID, "msg-"+name))

			got := readDeletedFromSourceAt(t, st, sourceID, "msg-"+name)
			require.True(t, got.Valid, "tombstone must remain set")
			assert.Equal(t, seeded, got.String,
				"re-marking an already-tombstoned message must not rewrite the original stamp")
		})
	}
}

// TestTombstoneWriters_StillSetFreshStamp is the no-regression check: a
// not-yet-tombstoned message must still get stamped.
func TestTombstoneWriters_StillSetFreshStamp(t *testing.T) {
	writers := tombstoneWriters()
	for _, name := range sortedWriterNames() {
		writer := writers[name]
		t.Run(name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			sourceID := seedTombstoneMessage(t, st, "fresh-"+name, "msg-"+name)

			before := readDeletedFromSourceAt(t, st, sourceID, "msg-"+name)
			require.False(t, before.Valid, "precondition: message starts undeleted")

			require.NoError(t, writer(t, st, sourceID, "msg-"+name))

			after := readDeletedFromSourceAt(t, st, sourceID, "msg-"+name)
			assert.True(t, after.Valid, "a not-yet-tombstoned message must still be stamped")
		})
	}
}

// TestTombstoneWriters_DoNotBumpLastModifiedForAlreadyTombstoned pins the
// other half of the idempotency contract: the guard must make the UPDATE
// affect zero rows for an already-tombstoned message, so the
// trg_messages_last_modified (SQLite) / set_messages_last_modified (PG)
// trigger never fires and last_modified is not bumped. Before the fix, every
// re-scan of the already-deleted population would bump last_modified,
// forcing incremental consumers to reprocess it on every run.
func TestTombstoneWriters_DoNotBumpLastModifiedForAlreadyTombstoned(t *testing.T) {
	writers := tombstoneWriters()
	for _, name := range sortedWriterNames() {
		writer := writers[name]
		t.Run(name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			sourceID := seedTombstoneMessage(t, st, "lm-"+name, "msg-"+name)
			setDeletedFromSourceAt(t, st, sourceID, "msg-"+name)

			var id int64
			require.NoError(t, st.DB().QueryRow(
				st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`),
				sourceID, "msg-"+name,
			).Scan(&id), "look up message id")
			base := baselineLM(t, st, id)

			require.NoError(t, writer(t, st, sourceID, "msg-"+name))

			got := readLM(t, st, id)
			assert.Equal(t, base, got,
				"re-marking an already-tombstoned message must not bump last_modified")
		})
	}
}

// batchTombstoneWriter adapts the three batch-capable writers to accept two
// IDs at once, for the mixed-batch test below.
type batchTombstoneWriter func(t *testing.T, st *store.Store, sourceID int64, ids []string) error

func batchTombstoneWriters() map[string]batchTombstoneWriter {
	return map[string]batchTombstoneWriter{
		"MarkMessagesDeletedBatch": func(t *testing.T, st *store.Store, sourceID int64, ids []string) error {
			t.Helper()
			return st.MarkMessagesDeletedBatch(sourceID, ids)
		},
		"MarkMessagesDeletedFromReader": func(t *testing.T, st *store.Store, sourceID int64, ids []string) error {
			t.Helper()
			return st.MarkMessagesDeletedFromReader(sourceID, strings.NewReader(strings.Join(ids, "\n")+"\n"), 10)
		},
		"MarkMessagesDeletedByGmailIDBatch": func(t *testing.T, st *store.Store, sourceID int64, ids []string) error {
			t.Helper()
			return st.MarkMessagesDeletedByGmailIDBatch(ids)
		},
	}
}

// TestTombstoneBatchWriters_MixedBatchOnlyTombstonesFreshRows verifies a
// batch containing both an already-tombstoned row and a fresh row tombstones
// only the fresh one, leaving the existing stamp on the other untouched -
// the exact shape of a real repair-scan re-run against a partially-deleted
// population.
func TestTombstoneBatchWriters_MixedBatchOnlyTombstonesFreshRows(t *testing.T) {
	writers := batchTombstoneWriters()
	names := make([]string, 0, len(writers))
	for name := range writers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		writer := writers[name]
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			sourceID := seedTombstoneMessage(t, st, "mixed-"+name, "msg-already")
			source, err := st.GetOrCreateSource("gmail", "mixed-"+name)
			require.NoError(err)
			convID, err := st.EnsureConversationWithType(source.ID, "conv-mixed-"+name, "channel", "general")
			require.NoError(err)
			insertStoreTestMessage(t, st, sourceID, convID, "msg-fresh")

			seeded := setDeletedFromSourceAt(t, st, sourceID, "msg-already")

			require.NoError(writer(t, st, sourceID, []string{"msg-already", "msg-fresh"}))

			already := readDeletedFromSourceAt(t, st, sourceID, "msg-already")
			require.True(already.Valid)
			assert.Equal(seeded, already.String, "already-tombstoned row's stamp must survive a mixed batch")

			fresh := readDeletedFromSourceAt(t, st, sourceID, "msg-fresh")
			assert.True(fresh.Valid, "fresh row in the same batch must still be tombstoned")
		})
	}
}

// TestMarkMessageDeleted_ClearThenReMarkSetsFreshStamp pins the legitimate
// delete -> reappear -> re-delete cycle: ClearMessageDeletedFromSource must
// still be able to hand a message back to the guarded writers and get a NEW
// stamp, not have the guard hold it back. The sentinel is stamped on the
// message's FIRST tombstone (bypassing the writer, as in the other tests
// here) so the final stamp can be checked against something far away from
// "now" without depending on real-clock resolution: after Clear (NULL) and a
// re-mark, the guard's "AND deleted_from_source_at IS NULL" condition matches
// again and must produce a value that is neither NULL nor the old sentinel.
func TestMarkMessageDeleted_ClearThenReMarkSetsFreshStamp(t *testing.T) {
	writers := tombstoneWriters()
	for _, name := range sortedWriterNames() {
		writer := writers[name]
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			sourceID := seedTombstoneMessage(t, st, "cycle-"+name, "msg-"+name)

			// First deletion, immediately overwritten with a sentinel so the
			// later "fresh stamp" assertion doesn't depend on clock resolution.
			require.NoError(writer(t, st, sourceID, "msg-"+name))
			seeded := setDeletedFromSourceAt(t, st, sourceID, "msg-"+name)

			// Reappears upstream.
			require.NoError(st.ClearMessageDeletedFromSource(sourceID, "msg-"+name))
			cleared := readDeletedFromSourceAt(t, st, sourceID, "msg-"+name)
			require.False(cleared.Valid, "Clear must NULL the tombstone")

			// Repair scan reports it deleted again.
			require.NoError(writer(t, st, sourceID, "msg-"+name))

			got := readDeletedFromSourceAt(t, st, sourceID, "msg-"+name)
			require.True(got.Valid, "re-mark after Clear must set a tombstone")
			assert.NotEqual(t, seeded, got.String,
				"re-mark after Clear must set a NEW stamp, not be blocked by the guard")
		})
	}
}
