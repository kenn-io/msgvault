package store_test

import (
	"bytes"
	"log/slog"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// A deferred native trigger fails at Commit, after the Events clock has been
// touched. Driver diagnostics may contain callback or encrypted-state values.
func TestMCPEventsFailedWriterCommitDoesNotLogDriverText(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL deferred constraint trigger observes the commit boundary")
	}
	_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	const marker = "synthetic-private-commit-marker"
	_, err = f.Store.DB().Exec(`CREATE FUNCTION reject_mcp_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic-private-commit-marker'; END $$`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`CREATE CONSTRAINT TRIGGER reject_mcp_commit AFTER UPDATE ON mcp_event_clock DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_mcp_commit()`)
	require.NoError(err)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "synthetic-failed-commit", MessageType: "email"})
	require.Error(err)
	assert.NotContains(err.Error(), marker)
	assert.NotContains(logs.String(), marker)
	var count int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='synthetic-failed-commit'`).Scan(&count))
	assert.Zero(count)
}
