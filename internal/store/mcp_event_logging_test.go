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
	_, err = f.Store.DB().Exec(`CREATE FUNCTION reject_mcp_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.head_seq <> OLD.head_seq THEN RAISE EXCEPTION 'synthetic-private-commit-marker'; END IF; RETURN NEW; END $$`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`CREATE CONSTRAINT TRIGGER reject_mcp_commit AFTER UPDATE ON mcp_event_clock DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_mcp_commit()`)
	require.NoError(err)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	view := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
	_, err = view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID("synthetic-failed-commit").Build()})
	require.Error(err)
	assert.NotContains(err.Error(), marker)
	assert.NotContains(logs.String(), marker)
	var count int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='synthetic-failed-commit'`).Scan(&count))
	assert.Zero(count)
}

func TestMCPEventsUnrelatedCommitErrorKeepsDriverText(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	_, err = f.Store.DB().Exec(`CREATE TABLE synthetic_commit_parent (id INTEGER PRIMARY KEY)`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`CREATE TABLE synthetic_commit_child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL, FOREIGN KEY(parent_id) REFERENCES synthetic_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
	require.NoError(err)
	if f.Store.IsPostgreSQL() {
		_, err = f.Store.DB().Exec(`CREATE FUNCTION synthetic_reject_message_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO synthetic_commit_child(id,parent_id) VALUES (1,999); RETURN NEW; END $$`)
		require.NoError(err)
		_, err = f.Store.DB().Exec(`CREATE TRIGGER synthetic_reject_message_commit AFTER INSERT ON messages FOR EACH ROW EXECUTE FUNCTION synthetic_reject_message_commit()`)
	} else {
		_, err = f.Store.DB().Exec(`CREATE TRIGGER synthetic_reject_message_commit AFTER INSERT ON messages BEGIN INSERT INTO synthetic_commit_child(id,parent_id) VALUES (1,999); END`)
	}
	require.NoError(err)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	backfill := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill})
	_, err = backfill.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "synthetic-unrelated-commit-failure", MessageType: "email"})
	require.Error(err)
	assert.NotContains(err.Error(), "events_storage_unavailable")
	assert.NotContains(logs.String(), "events_storage_unavailable")
	assert.Contains(logs.String(), "sql tx commit failed")
	var count int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='synthetic-unrelated-commit-failure'`).Scan(&count))
	assert.Zero(count)
	var eventCount int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&eventCount))
	assert.Zero(eventCount)
}
