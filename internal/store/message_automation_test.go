package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func assertAutomation(t *testing.T, st *store.Store, id int64, want uint32, known bool) {
	t.Helper()
	flags, current, err := st.MessageAutomationContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, known, current)
	assert.Equal(t, want, flags)
	var partial int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE
 (automation_flags IS NULL AND automation_rule_version IS NOT NULL) OR
 (automation_flags IS NOT NULL AND automation_rule_version IS NULL)`).Scan(&partial))
	assert.Zero(t, partial)
}

func TestPersistMessageWritesAutomation(t *testing.T) {
	f := storetest.New(t)
	flags := uint32(4)
	data := &store.MessagePersistData{Message: &store.Message{ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "automation", MessageType: "email"},
		RawMIME: []byte("List-Id: <news.example.test>\r\n\r\nbody"), Automation: &flags}
	id, err := f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(t, err)
	assertAutomation(t, f.Store, id, 4, true)
	// Same raw preserves existing columns when no new parse was supplied.
	data.Automation = nil
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(t, err)
	assertAutomation(t, f.Store, id, 4, true)
	// Raw replacement invalidates first; freshly parsed zero is then published.
	flags = 0
	data.Automation = &flags
	data.RawMIME = []byte("Subject: human\r\n\r\nbody")
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(t, err)
	assertAutomation(t, f.Store, id, 0, true)
}

func TestRawMIMEChangeInvalidatesAutomation(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("raw")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("Subject: first\r\n\r\nbody")))
	require.NoError(t, f.Store.PutMessageAutomationContext(t.Context(), id, 4))
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("Subject: first\r\n\r\nbody")))
	assertAutomation(t, f.Store, id, 4, true)
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("Subject: changed\r\n\r\nbody")))
	assertAutomation(t, f.Store, id, 0, false)
	require.NoError(t, f.Store.PutMessageAutomationContext(t.Context(), id, 0))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), id)
	require.NoError(t, err)
	assertAutomation(t, f.Store, id, 0, false)
}

func TestAutomationWriteLeavesContentChangedAt(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("content")
	settleFeedClock(t, f.Store)
	consumer := newChangeFeedConsumer()
	consumer.drain(t, f.Store)
	var before, after string
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE id = ?`), id).Scan(&before))
	require.NoError(t, f.Store.PutMessageAutomationContext(t.Context(), id, 12))
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE id = ?`), id).Scan(&after))
	assert.Equal(t, before, after)
	assert.Empty(t, consumer.mustPoll(t, f.Store).Messages)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET automation_flags = NULL, automation_rule_version = NULL WHERE id = ?`), id)
	require.NoError(t, err)
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE id = ?`), id).Scan(&after))
	assert.Equal(t, before, after)
	assert.Empty(t, consumer.mustPoll(t, f.Store).Messages)
}

func TestStaleRuleVersionReadsAsNotCurrentAutomation(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("stale")
	require.NoError(t, f.Store.PutMessageAutomationContext(t.Context(), id, 4))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET automation_rule_version = 1 WHERE id = ?`), id)
	require.NoError(t, err)
	assertAutomation(t, f.Store, id, 0, false)
}

func TestCopySubsetCopiesAutomationColumns(t *testing.T) {
	testutil.SkipIfPostgres(t, "subset copies SQLite archives")
	f := storetest.New(t)
	id := f.CreateMessage("copy")
	require.NoError(t, f.Store.PutMessageAutomationContext(t.Context(), id, 8))
	dir := t.TempDir()
	_, err := store.CopySubset(t.Context(), store.DBPathForTest(f.Store), dir, 10, true)
	require.NoError(t, err)
	copied, err := store.Open(filepath.Join(dir, "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, copied.Close()) })
	assertAutomation(t, copied, id, 8, true)
}

func TestRepairAutomationFlagsDryRunApplyAndEmbeddingCoverage(t *testing.T) {
	f := storetest.New(t)
	for _, tc := range []struct{ id, raw string }{
		{"bulk", "From: person@example.test\r\nList-Id: <news.example.test>\r\nList-Unsubscribe: <mailto:leave@example.test>\r\n\r\nbody"},
		{"human", "From: person@example.test\r\nSubject: hello\r\n\r\nbody"},
		{"calendar", "From: person@example.test\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b\r\nContent-Type: text/calendar\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n--b--\r\n"},
		{"invalid", "invalid MIME"},
	} {
		id := f.CreateMessage(tc.id)
		require.NoError(t, f.Store.UpsertMessageRaw(id, []byte(tc.raw)))
		require.NoError(t, f.Store.SetEmbedGen(t.Context(), []int64{id}, 7))
	}
	f.CreateMessage("missing")
	before, err := f.Store.DerivedDataRevisionContext(t.Context())
	require.NoError(t, err)
	_, _, _, missingBefore, err := f.Store.CoverageCounts(t.Context(), 7)
	require.NoError(t, err)
	dry, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{BatchSize: 1}, nil)
	require.NoError(t, err)
	assert.Equal(t, store.AutomationRepairSummary{Scanned: 5, Written: 3, Bulk: 1, Automated: 2, Unreadable: 2, Remaining: 5}, dry)
	rev, err := f.Store.DerivedDataRevisionContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, before, rev)
	applied, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true, BatchSize: 1}, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(3), applied.Written)
	assert.Equal(t, int64(2), applied.Remaining)
	rev, err = f.Store.DerivedDataRevisionContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, before+1, rev)
	_, _, _, missingAfter, err := f.Store.CoverageCounts(t.Context(), 7)
	require.NoError(t, err)
	assert.Equal(t, missingBefore, missingAfter)
	again, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true}, nil)
	require.NoError(t, err)
	assert.Zero(t, again.Written)
	assert.Equal(t, int64(2), again.Scanned)
	rev, err = f.Store.DerivedDataRevisionContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, before+1, rev)
}

func TestRepairAutomationFlagsApplyIsResumable(t *testing.T) {
	f := storetest.New(t)
	for _, id := range f.CreateMessages(3) {
		require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("From: person@example.test\r\n\r\nbody")))
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first, err := f.Store.RepairAutomationFlags(ctx, store.AutomationRepairOptions{Apply: true, BatchSize: 1}, func(store.AutomationRepairSummary) { cancel() })
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(1), first.Written)
	rest, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true, BatchSize: 1}, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(2), rest.Written)
	assert.Zero(t, rest.Remaining)
}

func TestUpsertAutomationColumnsBothOrNeither(t *testing.T) {
	f := storetest.New(t)
	msg := &store.Message{ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "upsert", MessageType: "email", AutomationFlags: sql.NullInt64{Int64: 4, Valid: true}, AutomationRuleVersion: sql.NullInt64{Int64: store.AutomationRuleVersion, Valid: true}}
	id, err := f.Store.UpsertMessageContext(t.Context(), msg)
	require.NoError(t, err)
	assertAutomation(t, f.Store, id, 4, true)
	msg.AutomationFlags = sql.NullInt64{}
	msg.AutomationRuleVersion = sql.NullInt64{}
	_, err = f.Store.UpsertMessageContext(t.Context(), msg)
	require.NoError(t, err)
	assertAutomation(t, f.Store, id, 4, true)
	msg.AutomationFlags = sql.NullInt64{Int64: 0, Valid: true}
	msg.AutomationRuleVersion = sql.NullInt64{Int64: store.AutomationRuleVersion, Valid: true}
	_, err = f.Store.UpsertMessageContext(t.Context(), msg)
	require.NoError(t, err)
	assertAutomation(t, f.Store, id, 0, true)
}

func TestRepairAutomationFlagsSkipsCurrentAndChatAndRepairsStale(t *testing.T) {
	f := storetest.New(t)
	current := f.CreateMessage("current")
	stale := f.CreateMessage("stale")
	chat := f.CreateMessage("chat")
	for _, id := range []int64{current, stale, chat} {
		require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("Subject: hello\r\n\r\nbody")))
	}
	require.NoError(t, f.Store.PutMessageAutomationContext(t.Context(), current, 0))
	require.NoError(t, f.Store.PutMessageAutomationContext(t.Context(), stale, 4))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET automation_rule_version = 1 WHERE id = ?`), stale)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type = 'slack' WHERE id = ?`), chat)
	require.NoError(t, err)
	sum, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), sum.Scanned)
	assert.Equal(t, int64(1), sum.Written)
	assert.Zero(t, sum.Remaining)
	assertAutomation(t, f.Store, stale, 0, true)
	assertAutomation(t, f.Store, chat, 0, false)
	for _, size := range []int{-1, 5001} {
		_, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{BatchSize: size}, nil)
		require.Error(t, err)
	}
}

func TestAutomationSchemaMigratesMain39(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("legacy")
	if f.Store.IsPostgreSQL() {
		_, err := f.Store.DB().ExecContext(t.Context(), `DROP TRIGGER trg_messages_automation_raw ON message_raw`)
		require.NoError(t, err)
	} else {
		_, err := f.Store.DB().ExecContext(t.Context(), `DROP TRIGGER trg_messages_automation_raw_update; DROP TRIGGER trg_messages_automation_raw_delete`)
		require.NoError(t, err)
	}
	for _, query := range []string{`DROP INDEX idx_messages_automation_flags`, `ALTER TABLE messages DROP COLUMN automation_flags`, `ALTER TABLE messages DROP COLUMN automation_rule_version`, `UPDATE archive_metadata SET value = '39' WHERE key = 'schema_version'`} {
		_, err := f.Store.DB().ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	versionBefore, err := f.Store.SchemaVersionContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 39, versionBefore)
	require.ErrorContains(t, f.Store.ValidateSchemaContext(t.Context()), "run setup")
	require.NoError(t, f.Store.InitSchemaUnlessCurrentContext(t.Context()))
	require.NoError(t, f.Store.ValidateSchemaContext(t.Context()))
	var matrixIndex int
	indexQuery := `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_matrix_encrypted_events_pending_session'`
	if f.Store.IsPostgreSQL() {
		indexQuery = `SELECT count(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'idx_matrix_encrypted_events_pending_session'`
	}
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), indexQuery).Scan(&matrixIndex))
	assert.Equal(t, 1, matrixIndex)
	assertAutomation(t, f.Store, id, 0, false)
	require.NoError(t, f.Store.PutMessageAutomationContext(t.Context(), id, 4))
	assertAutomation(t, f.Store, id, 4, true)
	version, err := f.Store.SchemaVersionContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, store.SchemaVersion, version)
}

func TestAutomationRawCompletenessMarkerIsAtomic(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("raw-and-flags")
	require.NoError(t, f.Store.UpsertMessageRawWithAutomationContext(t.Context(), id, []byte(`{"text":"hello"}`), "slack_json", store.FlagBotSender))
	assertAutomation(t, f.Store, id, store.FlagBotSender, true)
	require.NoError(t, f.Store.UpsertMessageRawWithAutomationContext(t.Context(), id, []byte(`{"text":"human"}`), "slack_json", 0))
	assertAutomation(t, f.Store, id, 0, true)
}

func TestRepairAutomationFlagsRecoveredCSVAndNonpositiveIDs(t *testing.T) {
	f := storetest.New(t)
	insert := `INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type)`
	if f.Store.IsPostgreSQL() {
		insert += ` OVERRIDING SYSTEM VALUE`
	}
	insert += ` VALUES (?, ?, ?, ?, 'email')`
	for _, id := range []int64{-7, 0, 9} {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(insert), id, f.ConvID, f.Source.ID, fmt.Sprint(id))
		require.NoError(t, err)
		require.NoError(t, f.Store.UpsertMessageRaw(id, testemail.LegacyCSV(true, testemail.LegacyCSVOptions{})))
	}
	sum, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true, BatchSize: 1}, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(3), sum.Written)
	assert.Zero(t, sum.Unreadable)
	assert.Zero(t, sum.Remaining)
	for _, id := range []int64{-7, 0, 9} {
		assertAutomation(t, f.Store, id, 0, true)
	}
}

func TestAutomationUnknownRawInvalidationDoesNotWriteMessage(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("unknown-raw")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("Subject: old\r\n\r\nbody")))
	for _, remove := range []bool{false, true} {
		before := baselineLM(t, f.Store, id)
		if remove {
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), id)
			require.NoError(t, err)
		} else {
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_raw SET raw_data = ? WHERE message_id = ?`), []byte("changed"), id)
			require.NoError(t, err)
		}
		assert.Equal(t, before, readLM(t, f.Store, id))
	}
}

func TestRepairAutomationFlagsEmbeddingConsumerCAS(t *testing.T) {
	f := storetest.New(t)
	covered := f.CreateMessage("covered")
	pending := f.CreateMessage("pending")
	for _, id := range []int64{covered, pending} {
		require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("Subject: hello\r\n\r\nbody")))
	}
	require.NoError(t, f.Store.SetEmbedGen(t.Context(), []int64{covered}, 7))
	before := baselineLM(t, f.Store, pending)
	sum, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(2), sum.Written)
	missed, count, err := f.Store.SetEmbedGenIfUnchanged(t.Context(), []store.EmbedGenStamp{{ID: pending, LastModified: before}}, 7)
	require.NoError(t, err)
	assert.Equal(t, []int64{pending}, missed)
	assert.Zero(t, count)
	candidates, err := f.Store.ScanForEmbedding(t.Context(), 7, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, []int64{pending}, candidates, "covered rows stay excluded; only pending work needs a fresh token")
	missed, count, err = f.Store.SetEmbedGenIfUnchanged(t.Context(), []store.EmbedGenStamp{{ID: pending, LastModified: readLM(t, f.Store, pending)}}, 7)
	require.NoError(t, err)
	assert.Empty(t, missed)
	assert.Equal(t, 1, count)
}
