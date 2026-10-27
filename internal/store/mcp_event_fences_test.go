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
	if !f.Store.IsPostgreSQL() {
		t.Skip("SQLite admits one writer, so lock order cannot deadlock there")
	}
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
}
