package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopSchemaUpgradeAndRetry(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	s := f.Store
	columns := func() []string {
		rows, err := s.DB().Query("SELECT * FROM messages LIMIT 0")
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		cols, err := rows.Columns()
		require.NoError(t, err)
		require.NoError(t, rows.Err())
		return cols
	}
	before := columns()
	// Reset the prior completion marker and remove the new objects and the
	// triggers that can keep enqueueing work through this legacy upgrade.
	if s.IsPostgreSQL() {
		for _, trigger := range []struct{ name, table string }{
			{"trg_loop_queue_labels_insert", "message_labels"},
			{"trg_loop_queue_labels_update", "message_labels"},
			{"trg_loop_queue_labels_delete", "message_labels"},
			{"trg_loop_queue_recipients_insert", "message_recipients"},
			{"trg_loop_queue_recipients_update", "message_recipients"},
			{"trg_loop_queue_recipients_delete", "message_recipients"},
			{"trg_loop_queue_messages_move", "messages"},
			{"trg_loop_queue_conversation_type_update", "conversations"},
			{"trg_loop_queue_conversation_participants", "conversation_participants"},
		} {
			loopExec(t, s, "DROP TRIGGER IF EXISTS "+trigger.name+" ON "+trigger.table)
		}
	} else {
		for _, trigger := range []string{
			"trg_loop_queue_labels_insert", "trg_loop_queue_labels_update", "trg_loop_queue_labels_delete",
			"trg_loop_queue_recipients_insert", "trg_loop_queue_recipients_update", "trg_loop_queue_recipients_delete",
			"trg_loop_queue_messages_move", "trg_loop_queue_conversation_type_update",
			"trg_loop_queue_conversation_participants_insert", "trg_loop_queue_conversation_participants_update", "trg_loop_queue_conversation_participants_delete",
		} {
			loopExec(t, s, "DROP TRIGGER IF EXISTS "+trigger)
		}
	}
	for _, table := range []string{"open_loops", "loop_suppressions", "loop_conversation_queue", "message_automation_signals", "loop_derivation_state"} {
		loopExec(t, s, "DROP TABLE "+table)
	}
	loopExec(t, s, "DELETE FROM applied_migrations WHERE name='loop_projection_triggers_v1'")
	loopExec(t, s, "UPDATE archive_metadata SET value='10' WHERE key='schema_version'")
	requirements.NoError(s.InitSchemaContext(t.Context()))
	assertions.Equal(before, columns())
	version, err := s.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(store.SchemaVersion, version)
	held := insertSchemaLoop(t, f, "held", "reply_owed", "turn_state")
	done := insertSchemaLoop(t, f, "done", "awaiting_reply", "turn_state")
	loopExec(t, s, "UPDATE open_loops SET snoozed_until=?,agenda_state='created',fulfilment_suggested_at=CURRENT_TIMESTAMP WHERE id=?", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), held)
	loopExec(t, s, "UPDATE open_loops SET status='closed',closed_at=CURRENT_TIMESTAMP,closed_reason='done',closed_by='user' WHERE id=?", done)
	loopExec(t, s, "UPDATE archive_metadata SET value='10' WHERE key='schema_version'")
	// Inject a real database failure at the final completion stamp. This keeps
	// the test on the production path without depending on a Go test hook.
	if s.IsPostgreSQL() {
		loopExec(t, s, `CREATE FUNCTION reject_loop_test_schema_stamp() RETURNS trigger AS $$
		 BEGIN IF NEW.key='schema_version' THEN RAISE EXCEPTION 'test interruption'; END IF; RETURN NEW; END;
		 $$ LANGUAGE plpgsql`)
		loopExec(t, s, `CREATE TRIGGER trg_loop_test_schema_stamp BEFORE INSERT ON archive_metadata
		 FOR EACH ROW EXECUTE FUNCTION reject_loop_test_schema_stamp()`)
	} else {
		loopExec(t, s, `CREATE TRIGGER trg_loop_test_schema_stamp BEFORE INSERT ON archive_metadata
		 WHEN NEW.key='schema_version' BEGIN SELECT RAISE(ABORT,'test interruption'); END`)
	}
	requirements.ErrorContains(s.InitSchemaContext(t.Context()), "test interruption")
	if s.IsPostgreSQL() {
		loopExec(t, s, "DROP TRIGGER trg_loop_test_schema_stamp ON archive_metadata")
		loopExec(t, s, "DROP FUNCTION reject_loop_test_schema_stamp()")
	} else {
		loopExec(t, s, "DROP TRIGGER trg_loop_test_schema_stamp")
	}
	version, err = s.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(10, version, "interrupted setup must not stamp")
	requirements.NoError(s.InitSchemaContext(t.Context()))
	requirements.NoError(s.InitSchemaContext(t.Context()))
	var agenda, status, reason string
	var snoozed, fulfilment sql.NullTime
	requirements.NoError(s.DB().QueryRow(s.Rebind("SELECT agenda_state,snoozed_until,fulfilment_suggested_at FROM open_loops WHERE id=?"), held).Scan(&agenda, &snoozed, &fulfilment))
	assertions.Equal("created", agenda)
	assertions.True(snoozed.Valid)
	assertions.True(fulfilment.Valid)
	requirements.NoError(s.DB().QueryRow(s.Rebind("SELECT status,closed_reason FROM open_loops WHERE id=?"), done).Scan(&status, &reason))
	assertions.Equal("closed", status)
	assertions.Equal("done", reason)
	mid := f.CreateMessage("post-upgrade")
	loopExec(t, s, "DELETE FROM loop_conversation_queue")
	loopExec(t, s, "UPDATE messages SET source_is_from_me=TRUE WHERE id=?", mid)
	assertions.Equal(int64(2), queueRevision(t, s, f.ConvID))
	assertions.Equal(1, loopCount(t, s, "loop_derivation_state"))
}
