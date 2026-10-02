package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Observe actual index DDL in an isolated PostgreSQL schema. The observer does
// not replace or delay CREATE INDEX; it records the transaction's timeout.
func TestAccountAttributionMigrationLiftsPostgresStatementTimeout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	var superuser bool
	require.NoError(st.db.QueryRow(`SELECT rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&superuser))
	if !superuser {
		t.Skip("DDL observation requires PostgreSQL event-trigger permission")
	}
	var schema string
	require.NoError(st.db.QueryRow(`SELECT current_schema()`).Scan(&schema))
	_, err := st.db.Exec(`CREATE TABLE account_index_timeouts (value TEXT NOT NULL)`)
	require.NoError(err)
	_, err = st.db.Exec(`CREATE FUNCTION observe_account_index_timeout() RETURNS event_trigger LANGUAGE plpgsql AS $$
 BEGIN
   IF current_schema() = '` + schema + `' AND current_query() LIKE 'CREATE INDEX%idx_messages_account_%'
     OR current_schema() = '` + schema + `' AND current_query() LIKE 'CREATE INDEX%idx_messages_source_account%'
   THEN
     INSERT INTO account_index_timeouts VALUES (current_setting('statement_timeout'));
   END IF;
 END $$`)
	require.NoError(err)
	trigger := schema + "_account_indexes"
	_, err = st.db.Exec(`CREATE EVENT TRIGGER ` + trigger + ` ON ddl_command_start WHEN TAG IN ('CREATE INDEX') EXECUTE FUNCTION observe_account_index_timeout()`)
	require.NoError(err)
	t.Cleanup(func() { _, _ = st.db.Exec(`DROP EVENT TRIGGER IF EXISTS ` + trigger) })
	for _, name := range []string{"idx_messages_account_address", "idx_messages_source_account", "idx_messages_account_pending"} {
		_, err = st.db.Exec(`DROP INDEX ` + name)
		require.NoError(err)
	}
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name='account_attribution_schema_v1'`)
	require.NoError(err)
	require.NoError(st.ensureAccountAttributionSchema(context.Background()))
	var count, lifted int
	require.NoError(st.db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE value='0') FROM account_index_timeouts`).Scan(&count, &lifted))
	assert.Equal(3, count, "observe each real index creation")
	assert.Equal(3, lifted, "all archive-sized index builds must disable the pooled statement timeout")
	require.NoError(st.ensureAccountAttributionSchema(context.Background()))
	var replayCount int
	require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM account_index_timeouts`).Scan(&replayCount))
	assert.Equal(count, replayCount, "completed migration must be idempotent")
}
