package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
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
	// Replacing raw without parsed evidence invalidates the old flags.
	data.Automation = nil
	data.RawMIME = []byte("invalid MIME")
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(t, err)
	assertAutomation(t, f.Store, id, 0, false)
	// Raw replacement invalidates first; freshly parsed zero is then published.
	flags = 0
	data.Automation = &flags
	data.RawMIME = []byte("Subject: human\r\n\r\nbody")
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(t, err)
	assertAutomation(t, f.Store, id, 0, true)
}

func TestRawMIMEChangeInvalidatesAutomation(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("raw")
	require.NoError(f.Store.UpsertMessageRaw(id, []byte("Subject: first\r\n\r\nbody")))
	require.NoError(f.Store.PutMessageAutomationContext(t.Context(), id, 4))
	require.NoError(f.Store.UpsertMessageRaw(id, []byte("Subject: first\r\n\r\nbody")))
	assertAutomation(t, f.Store, id, 4, true)
	require.NoError(f.Store.UpsertMessageRaw(id, []byte("Subject: changed\r\n\r\nbody")))
	assertAutomation(t, f.Store, id, 0, false)
	require.NoError(f.Store.PutMessageAutomationContext(t.Context(), id, 0))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), id)
	require.NoError(err)
	assertAutomation(t, f.Store, id, 0, false)
}

func TestAutomationWriteLeavesContentChangedAt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("content")
	settleFeedClock(t, f.Store)
	consumer := newChangeFeedConsumer()
	consumer.drain(t, f.Store)
	var before, after string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE id = ?`), id).Scan(&before))
	require.NoError(f.Store.PutMessageAutomationContext(t.Context(), id, 12))
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE id = ?`), id).Scan(&after))
	assert.Equal(before, after)
	assert.Empty(consumer.mustPoll(t, f.Store).Messages)
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

func TestRepairAutomationFlagsDryRunApply(t *testing.T) {
	t.Run("original changes after parsing", func(t *testing.T) {
		for _, change := range []string{"raw", "missing", "format", "encryption", "compression", "current"} {
			t.Run(change, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := storetest.New(t)
				ids := f.CreateMessages(2)
				for _, id := range ids {
					require.NoError(f.Store.UpsertMessageRaw(id, []byte("From: person@example.test\r\nList-Id: <news.example.test>\r\n\r\nbody")))
				}
				statement := `UPDATE message_raw SET raw_data = ? WHERE message_id = ?`
				args := []any{ids[0]}
				if change == "raw" {
					args = []any{[]byte("From: person@example.test\r\n\r\nchanged"), ids[0]}
				}
				switch change {
				case "current":
					statement = `UPDATE messages SET automation_flags = 0, automation_rule_version = ? WHERE id = ?`
					args = []any{store.AutomationRuleVersion, ids[0]}
				case "missing":
					statement = `DELETE FROM message_raw WHERE message_id = ?`
				case "format":
					statement = `UPDATE message_raw SET raw_format = 'slack_json' WHERE message_id = ?`
				case "encryption":
					statement = `UPDATE message_raw SET encryption_version = 1 WHERE message_id = ?`
				case "compression":
					statement = `UPDATE message_raw SET compression = 'none' WHERE message_id = ?`
				}
				previous := slog.Default()
				reads := 0
				var mutationErr error
				slog.SetDefault(slog.New(automationRepairLogHook{
					Handler:     slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}),
					onStatement: func(loggedStatement string) {
						if strings.HasPrefix(loggedStatement, "SELECT raw_data, compression, raw_format") {
							reads++
						}
						if reads == 2 {
							reads++ // The first original is already parsed; the batch is not locked yet.
							_, mutationErr = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(statement), args...)
						}
					},
				}))
				t.Cleanup(func() { slog.SetDefault(previous) })
				sum, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true})
				require.NoError(err)
				require.NoError(mutationErr)
				require.GreaterOrEqual(reads, 3, "the mutation occurs after the first original was parsed")
				assert.Equal(int64(1), sum.Written)
				assert.Equal(int64(1), sum.Bulk)
				assert.Equal(int64(1), sum.Automated)
				remaining := int64(1)
				if change == "current" {
					remaining = 0
				}
				assert.Equal(remaining, sum.Remaining)
				assertAutomation(t, f.Store, ids[0], 0, change == "current")
				assertAutomation(t, f.Store, ids[1], 4, true)
			})
		}
	})
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	for _, tc := range []struct{ id, raw string }{
		{"bulk", "From: person@example.test\r\nList-Id: <news.example.test>\r\nList-Unsubscribe: <mailto:leave@example.test>\r\n\r\nbody"},
		{"human", "From: person@example.test\r\nSubject: hello\r\n\r\nbody"},
		{"calendar", "From: person@example.test\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b\r\nContent-Type: text/calendar\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n--b--\r\n"},
		{"invalid", "invalid MIME"},
	} {
		id := f.CreateMessage(tc.id)
		require.NoError(f.Store.UpsertMessageRaw(id, []byte(tc.raw)))
	}
	f.CreateMessage("missing")
	before, err := f.Store.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	dry, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{BatchSize: 1})
	require.NoError(err)
	assert.Equal(store.AutomationRepairSummary{Scanned: 5, Written: 3, Bulk: 1, Automated: 2, Unreadable: 2, Remaining: 5}, dry)
	rev, err := f.Store.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(before, rev)
	applied, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true, BatchSize: 1})
	require.NoError(err)
	assert.Equal(int64(3), applied.Written)
	assert.Equal(int64(2), applied.Remaining)
	rev, err = f.Store.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(before+1, rev)
	again, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true})
	require.NoError(err)
	assert.Zero(again.Written)
	assert.Equal(int64(2), again.Scanned)
	rev, err = f.Store.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(before+1, rev)
}

// automationRepairLogHook observes real SQL execution without a production hook.
// Its callback runs before the delegated handler's write lock.
type automationRepairLogHook struct {
	slog.Handler
	onStatement func(string)
}

func (h automationRepairLogHook) Handle(ctx context.Context, record slog.Record) error {
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "stmt" {
			h.onStatement(attr.Value.String())
		}
		return true
	})
	return h.Handler.Handle(ctx, record)
}

func TestRepairAutomationFlagsApplyIsResumable(t *testing.T) {
	for _, mode := range []string{"canceled", "failed batch"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			ids := f.CreateMessages(3)
			for _, id := range ids {
				require.NoError(f.Store.UpsertMessageRaw(id, []byte("From: person@example.test\r\n\r\nbody")))
			}
			before, err := f.Store.DerivedDataRevisionContext(t.Context())
			require.NoError(err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			drop := "DROP TRIGGER fail_automation_batch"
			if mode == "failed batch" {
				fault := fmt.Sprintf(`CREATE TRIGGER fail_automation_batch BEFORE UPDATE OF automation_flags ON messages WHEN NEW.id = %d BEGIN SELECT RAISE(ABORT, 'automation batch sentinel'); END`, ids[1])
				if f.Store.IsPostgreSQL() {
					_, err := f.Store.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION fail_automation_batch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id = %d THEN RAISE EXCEPTION 'automation batch sentinel'; END IF; RETURN NEW; END $$`, ids[1]))
					require.NoError(err)
					fault = `CREATE TRIGGER fail_automation_batch BEFORE UPDATE OF automation_flags ON messages FOR EACH ROW EXECUTE FUNCTION fail_automation_batch()`
					drop += " ON messages"
				}
				_, err := f.Store.DB().ExecContext(t.Context(), fault)
				require.NoError(err)
			}
			previous := slog.Default()
			scans := 0
			slog.SetDefault(slog.New(automationRepairLogHook{
				Handler:     slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}),
				onStatement: func(statement string) {
					if strings.HasPrefix(statement, "SELECT m.id FROM messages m WHERE") {
						scans++
						if mode == "canceled" && scans == 2 {
							cancel() // The next page is scanned only after batch one commits.
						}
					}
				},
			}))
			store.ConfigureSQLLogging(store.SQLLogOptions{SlowMs: -1})
			t.Cleanup(func() {
				slog.SetDefault(previous)
				store.ConfigureSQLLogging(store.SQLLogOptions{})
			})
			first, err := f.Store.RepairAutomationFlags(ctx, store.AutomationRepairOptions{Apply: true, BatchSize: 1})
			if mode == "canceled" {
				require.ErrorIs(err, context.Canceled)
			} else {
				require.ErrorContains(err, "automation batch sentinel")
				_, err := f.Store.DB().ExecContext(t.Context(), drop)
				require.NoError(err)
			}
			assert.Equal(int64(1), first.Written)
			revision, err := f.Store.DerivedDataRevisionContext(t.Context())
			require.NoError(err)
			assert.Equal(before+1, revision, "the committed prefix must publish even when a later batch fails")
			rest, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true, BatchSize: 1})
			require.NoError(err)
			assert.Equal(int64(2), rest.Written)
			assert.Zero(rest.Remaining)
		})
	}
}

func TestUpsertAutomationColumnsBothOrNeither(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	msg := &store.Message{ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "upsert", MessageType: "email", AutomationFlags: sql.NullInt64{Int64: 4, Valid: true}, AutomationRuleVersion: sql.NullInt64{Int64: store.AutomationRuleVersion, Valid: true}}
	id, err := f.Store.UpsertMessageContext(t.Context(), msg)
	require.NoError(err)
	assertAutomation(t, f.Store, id, 4, true)
	msg.AutomationFlags = sql.NullInt64{}
	msg.AutomationRuleVersion = sql.NullInt64{}
	_, err = f.Store.UpsertMessageContext(t.Context(), msg)
	require.NoError(err)
	assertAutomation(t, f.Store, id, 4, true)
	msg.AutomationFlags = sql.NullInt64{Int64: 0, Valid: true}
	msg.AutomationRuleVersion = sql.NullInt64{Int64: store.AutomationRuleVersion, Valid: true}
	_, err = f.Store.UpsertMessageContext(t.Context(), msg)
	require.NoError(err)
	assertAutomation(t, f.Store, id, 0, true)
	msg.AutomationRuleVersion = sql.NullInt64{}
	_, err = f.Store.UpsertMessageContext(t.Context(), msg)
	require.ErrorContains(err, "automation flags and rule version must both be present or absent")
}

func TestRepairAutomationFlagsSkipsCurrentAndChatAndRepairsStale(t *testing.T) {
	t.Run("invalid batch size", func(t *testing.T) {
		f := storetest.New(t)
		for _, size := range []int{-1, 5001} {
			_, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{BatchSize: size})
			require.Error(t, err)
		}
	})
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	current := f.CreateMessage("current")
	stale := f.CreateMessage("stale")
	chat := f.CreateMessage("chat")
	for _, id := range []int64{current, stale} {
		require.NoError(f.Store.UpsertMessageRaw(id, []byte("Subject: hello\r\n\r\nbody")))
	}
	require.NoError(f.Store.PutMessageAutomationContext(t.Context(), current, 0))
	require.NoError(f.Store.PutMessageAutomationContext(t.Context(), stale, 4))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET automation_rule_version = 1 WHERE id = ?`), stale)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type = 'slack' WHERE id = ?`), chat)
	require.NoError(err)
	sum, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true})
	require.NoError(err)
	assert.Equal(int64(1), sum.Scanned)
	assert.Equal(int64(1), sum.Written)
	assert.Zero(sum.Remaining)
	assertAutomation(t, f.Store, stale, 0, true)
	assertAutomation(t, f.Store, chat, 0, false)
}

func TestAutomationSchemaMigratesMain39(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("legacy")
	if f.Store.IsPostgreSQL() {
		_, err := f.Store.DB().ExecContext(t.Context(), `DROP TRIGGER trg_messages_automation_raw ON message_raw; DROP TRIGGER trg_loop_evidence_attachments ON attachments; DROP FUNCTION invalidate_loop_evidence_attachments()`)
		require.NoError(err)
	} else {
		_, err := f.Store.DB().ExecContext(t.Context(), `DROP TRIGGER trg_messages_automation_raw_update; DROP TRIGGER trg_messages_automation_raw_delete; DROP TRIGGER trg_loop_evidence_attachments_update; DROP TRIGGER trg_loop_evidence_attachments_delete`)
		require.NoError(err)
	}
	for _, query := range []string{`DROP INDEX idx_messages_automation_flags`, `ALTER TABLE messages DROP COLUMN automation_flags`, `ALTER TABLE messages DROP COLUMN automation_rule_version`, `UPDATE archive_metadata SET value = '39' WHERE key = 'schema_version'`} {
		_, err := f.Store.DB().ExecContext(t.Context(), query)
		require.NoError(err)
	}
	versionBefore, err := f.Store.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(39, versionBefore)
	require.ErrorContains(f.Store.ValidateSchemaContext(t.Context()), "run setup")
	require.NoError(f.Store.InitSchemaUnlessCurrentContext(t.Context()))
	require.NoError(f.Store.ValidateSchemaContext(t.Context()))
	assertAutomation(t, f.Store, id, 0, false)
	require.NoError(f.Store.PutMessageAutomationContext(t.Context(), id, 4))
	assertAutomation(t, f.Store, id, 4, true)
	version, err := f.Store.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)
}

func TestAutomationRawPublicationWritesFlagsAfterRaw(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("raw-and-flags")
	require.NoError(t, f.Store.UpsertMessageRawWithAutomationContext(t.Context(), id, []byte(`{"text":"hello"}`), "slack_json", store.FlagBotSender))
	assertAutomation(t, f.Store, id, store.FlagBotSender, true)
	require.NoError(t, f.Store.UpsertMessageRawWithAutomationContext(t.Context(), id, []byte(`{"text":"human"}`), "slack_json", 0))
	assertAutomation(t, f.Store, id, 0, true)
}

func TestRepairAutomationFlagsRecoveredCSVAndNonpositiveIDs(t *testing.T) {
	for _, mode := range []string{"ordinary IDs", "keyset starts before zero"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			var ids []int64
			if mode == "ordinary IDs" {
				ids = f.CreateMessages(3)
			} else {
				for _, id := range []int64{-7, 0, 9} {
					ids = append(ids, insertMessageAtID(t, f, id, strconv.FormatInt(id, 10)))
				}
			}
			for _, id := range ids {
				require.NoError(f.Store.UpsertMessageRaw(id, testemail.LegacyCSV(true, testemail.LegacyCSVOptions{})))
			}
			sum, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true, BatchSize: 1})
			require.NoError(err)
			assert.Equal(int64(3), sum.Written)
			assert.Zero(sum.Unreadable)
			assert.Zero(sum.Remaining)
			for _, id := range ids {
				assertAutomation(t, f.Store, id, 0, true)
			}
		})
	}
}

func TestAutomationUnknownRawInvalidationDoesNotWriteMessage(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("unknown-raw")
	require.NoError(f.Store.UpsertMessageRaw(id, []byte("Subject: old\r\n\r\nbody")))
	for _, remove := range []bool{false, true} {
		before := baselineLM(t, f.Store, id)
		if remove {
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), id)
			require.NoError(err)
		} else {
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_raw SET raw_data = ? WHERE message_id = ?`), []byte("changed"), id)
			require.NoError(err)
		}
		assert.Equal(t, before, readLM(t, f.Store, id))
	}
}

func TestRepairAutomationFlagsEmbeddingConsumerCAS(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	covered := f.CreateMessage("covered")
	pending := f.CreateMessage("pending")
	for _, id := range []int64{covered, pending} {
		require.NoError(f.Store.UpsertMessageRaw(id, []byte("Subject: hello\r\n\r\nbody")))
	}
	require.NoError(f.Store.SetEmbedGen(t.Context(), []int64{covered}, 7))
	before := baselineLM(t, f.Store, pending)
	sum, err := f.Store.RepairAutomationFlags(t.Context(), store.AutomationRepairOptions{Apply: true})
	require.NoError(err)
	assert.Equal(int64(2), sum.Written)
	missed, count, err := f.Store.SetEmbedGenIfUnchanged(t.Context(), []store.EmbedGenStamp{{ID: pending, LastModified: before}}, 7)
	require.NoError(err)
	assert.Equal([]int64{pending}, missed)
	assert.Zero(count)
	candidates, err := f.Store.ScanForEmbedding(t.Context(), 7, 0, 10)
	require.NoError(err)
	assert.Equal([]int64{pending}, candidates, "covered rows stay excluded; only pending work needs a fresh token")
	missed, count, err = f.Store.SetEmbedGenIfUnchanged(t.Context(), []store.EmbedGenStamp{{ID: pending, LastModified: readLM(t, f.Store, pending)}}, 7)
	require.NoError(err)
	assert.Empty(missed)
	assert.Equal(1, count)
}
