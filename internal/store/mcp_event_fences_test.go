package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Real database triggers observe the transaction's lock order. The clock
// update must happen, and it must follow the identity fence in that transaction.
func TestMCPEventsEnabledWriterFencesIdentityBeforeClock(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
	require.NoError(err)
	_, err = f.Store.DB().Exec(`INSERT INTO archive_metadata (key,value) VALUES ('synthetic_mcp_identity_seen','0'),('synthetic_mcp_clock_seen','0')`)
	require.NoError(err)
	if f.Store.IsPostgreSQL() {
		_, err = f.Store.DB().Exec(`CREATE FUNCTION observe_mcp_identity() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='identity_revision' THEN UPDATE archive_metadata SET value=txid_current()::text WHERE key='synthetic_mcp_identity_seen'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = f.Store.DB().Exec(`CREATE TRIGGER observe_mcp_identity AFTER UPDATE ON archive_metadata FOR EACH ROW EXECUTE FUNCTION observe_mcp_identity()`)
		require.NoError(err)
		_, err = f.Store.DB().Exec(`CREATE FUNCTION observe_mcp_clock() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF (SELECT value FROM archive_metadata WHERE key='synthetic_mcp_identity_seen')<>txid_current()::text THEN RAISE EXCEPTION 'identity fence must precede Events clock'; END IF; UPDATE archive_metadata SET value='1' WHERE key='synthetic_mcp_clock_seen'; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = f.Store.DB().Exec(`CREATE TRIGGER observe_mcp_clock BEFORE UPDATE ON mcp_event_clock FOR EACH ROW EXECUTE FUNCTION observe_mcp_clock()`)
		require.NoError(err)
	} else {
		_, err = f.Store.DB().Exec(`CREATE TRIGGER observe_mcp_identity AFTER UPDATE ON archive_metadata WHEN NEW.key='identity_revision' BEGIN UPDATE archive_metadata SET value='1' WHERE key='synthetic_mcp_identity_seen'; END`)
		require.NoError(err)
		_, err = f.Store.DB().Exec(`CREATE TRIGGER observe_mcp_clock BEFORE UPDATE ON mcp_event_clock BEGIN SELECT CASE WHEN (SELECT value FROM archive_metadata WHERE key='synthetic_mcp_identity_seen')='1' THEN NULL ELSE RAISE(ABORT,'identity fence must precede Events clock') END; UPDATE archive_metadata SET value='1' WHERE key='synthetic_mcp_clock_seen'; END`)
		require.NoError(err)
	}
	assert.Positive(f.CreateMessage("synthetic-fenced-message"))
	var clockSeen string
	require.NoError(f.Store.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='synthetic_mcp_clock_seen'`).Scan(&clockSeen))
	assert.Equal("1", clockSeen)
}
