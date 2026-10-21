package store_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// The writer must acquire the identity fence before the Events clock.
func TestMCPEventsEnabledWriterFencesIdentityBeforeClock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
	require.NoError(err)
	if f.Store.IsPostgreSQL() {
		releaseIdentity := holdIdentityRow(t, f.Store)
		clockTx, err := f.Store.DB().BeginTx(t.Context(), nil)
		require.NoError(err)
		defer func() { _ = clockTx.Rollback() }()
		_, err = clockTx.Exec(`SELECT head_seq FROM mcp_event_clock WHERE singleton=1 FOR UPDATE`)
		require.NoError(err)
		done := make(chan error, 1)
		go func() {
			_, err := f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "synthetic-fenced-message", MessageType: "email"})
			done <- err
		}()
		waitForLockWait(t, f.Store, "archive_metadata", "writer must first wait for identity")
		releaseIdentity()
		waitForLockWait(t, f.Store, "UPDATE mcp_event_clock", "writer must next wait for the Events clock")
		probe, err := f.Store.DB().BeginTx(t.Context(), nil)
		require.NoError(err)
		defer func() { _ = probe.Rollback() }()
		_, err = probe.Exec(`SELECT value FROM archive_metadata WHERE key='identity_revision' FOR UPDATE NOWAIT`)
		var pgErr *pgconn.PgError
		require.ErrorAs(err, &pgErr)
		assert.Equal("55P03", pgErr.Code, "writer must hold identity while waiting for the clock")
		require.NoError(probe.Rollback())
		require.NoError(clockTx.Commit())
		assertNoDeadlock(t, waitResult(t, done, "enabled writer"))
		return
	}
	// SQLite admits one writer; triggers observe the order within its transaction.
	_, err = f.Store.DB().Exec(`INSERT INTO archive_metadata (key,value) VALUES ('synthetic_mcp_identity_seen','0'),('synthetic_mcp_clock_seen','0')`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`CREATE TRIGGER observe_mcp_identity AFTER UPDATE ON archive_metadata WHEN NEW.key='identity_revision' BEGIN UPDATE archive_metadata SET value='1' WHERE key='synthetic_mcp_identity_seen'; END`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`CREATE TRIGGER observe_mcp_clock BEFORE UPDATE ON mcp_event_clock BEGIN SELECT CASE WHEN (SELECT value FROM archive_metadata WHERE key='synthetic_mcp_identity_seen')='1' THEN NULL ELSE RAISE(ABORT,'identity fence must precede Events clock') END; UPDATE archive_metadata SET value='1' WHERE key='synthetic_mcp_clock_seen'; END`)
	require.NoError(err)
	assert.Positive(f.CreateMessage("synthetic-fenced-message"))
	var clockSeen string
	require.NoError(f.Store.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='synthetic_mcp_clock_seen'`).Scan(&clockSeen))
	assert.Equal("1", clockSeen)
}
