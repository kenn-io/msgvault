package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportSnapshotFailureLeavesNoConsumedDecision(t *testing.T) {
	for _, table := range []string{"message_bodies", "message_raw", "import_message_decisions", "mcp_event_log"} {
		t.Run(table, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			f := storetest.New(t)
			const owner = "owner:synthetic-import-rollback"
			_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
			require.NoError(err)
			view, finish := newSyntheticImportView(t, f, owner, true)
			// Fixed table names target the actual required SQL write, not a fake Store.
			if f.Store.IsPostgreSQL() {
				_, err = f.Store.DB().ExecContext(t.Context(), `CREATE FUNCTION synthetic_import_write_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required-write failure'; END $$`)
				require.NoError(err)
				_, err = f.Store.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER synthetic_import_write_failure BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION synthetic_import_write_failure()`, table))
			} else {
				_, err = f.Store.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER synthetic_import_write_failure BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT,'synthetic required-write failure'); END`, table))
			}
			require.NoError(err)
			message := f.NewMessage().WithSourceMessageID("synthetic-retryable-import").Build()
			data := &store.MessagePersistData{Message: message, BodyText: sql.NullString{String: "Synthetic required body", Valid: true}, RawMIME: []byte("Subject: Synthetic required raw\r\n\r\nSynthetic required body")}
			_, err = view.PersistMessageContext(t.Context(), data)
			require.Error(err)
			for _, query := range []string{`SELECT COUNT(*) FROM messages`, `SELECT COUNT(*) FROM message_bodies`, `SELECT COUNT(*) FROM message_raw`, `SELECT COUNT(*) FROM import_message_decisions`, `SELECT COUNT(*) FROM mcp_event_log`} {
				var count int
				require.NoError(f.Store.DB().QueryRowContext(t.Context(), query).Scan(&count))
				assert.Zero(count, "failed required snapshot must leave no message or consumed import decision")
			}
			if f.Store.IsPostgreSQL() {
				_, err = f.Store.DB().ExecContext(t.Context(), `DROP TRIGGER synthetic_import_write_failure ON `+table)
			} else {
				_, err = f.Store.DB().ExecContext(t.Context(), `DROP TRIGGER synthetic_import_write_failure`)
			}
			require.NoError(err)
			id, err := view.PersistMessageContext(t.Context(), data)
			require.NoError(err)
			require.Positive(id)
			finish()
			var count int
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&count))
			assert.Equal(1, count, "a failed precommit attempt remains eligible for one successful retry")
		})
	}
}
