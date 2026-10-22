package importer

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportEmlxLogicalRecoveryBeforeFirstCheckpointKeepsFrozenChoice(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-emlx-recovery", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "apple-mail", Kinds: []string{"message"}}}})
	require.NoError(err)
	root := filepath.Join(t.TempDir(), "Mail")
	messages := filepath.Join(root, "Mailboxes", "Synthetic.mbox", "Messages")
	require.NoError(os.MkdirAll(messages, 0o700))
	mkEmlx(t, messages, "1.emlx", []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <synthetic-emlx-recovery@example.net>\r\nSubject: Synthetic recovery\r\n\r\nSynthetic recovery body"))
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION reject_synthetic_emlx_checkpoint() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.cursor_before IS NOT NULL AND (OLD.cursor_before IS NULL OR OLD.cursor_before='') THEN RAISE EXCEPTION 'synthetic checkpoint rejection'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_synthetic_emlx_checkpoint BEFORE UPDATE OF cursor_before ON sync_runs FOR EACH ROW EXECUTE FUNCTION reject_synthetic_emlx_checkpoint()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_synthetic_emlx_checkpoint BEFORE UPDATE OF cursor_before ON sync_runs WHEN NEW.cursor_before IS NOT NULL AND (OLD.cursor_before IS NULL OR OLD.cursor_before='') BEGIN SELECT RAISE(ABORT,'synthetic checkpoint rejection'); END`)
	}
	require.NoError(err)
	interrupted, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Cancel at the native ingest boundary after the real checkpoint write
	// failed. This proves recovery from missing progress, without a timed race.
	first, err := ImportEmlxDir(interrupted, st, root, EmlxImportOptions{Identifier: "owner@example.net", IngestFunc: func(context.Context, *store.Store, int64, string, string, []int64, string, string, []byte, time.Time, *slog.Logger) error {
		cancel()
		return context.Canceled
	}})
	require.ErrorIs(err, context.Canceled)
	require.NotNil(first)
	require.NotEmpty(first.ImportRunID)
	var checkpoints int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sync_runs WHERE cursor_before IS NOT NULL AND cursor_before<>''`).Scan(&checkpoints))
	assert.Zero(checkpoints)
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER reject_synthetic_emlx_checkpoint ON sync_runs`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER reject_synthetic_emlx_checkpoint`)
	}
	require.NoError(err)
	_, err = ImportEmlxDir(t.Context(), st, root, EmlxImportOptions{Identifier: "owner@example.net", NotifyImports: true})
	require.Error(err, "missing progress cannot upgrade a frozen silent native operation")
	resumed, err := ImportEmlxDir(t.Context(), st, root, EmlxImportOptions{Identifier: "owner@example.net"})
	require.NoError(err)
	require.NotNil(resumed)
	require.False(resumed.HardErrors)
	assert.True(resumed.WasResumed)
	assert.Equal(first.ImportRunID, resumed.ImportRunID)
	assert.Equal(int64(1), resumed.MessagesAdded)
	var notifications int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log`).Scan(&notifications))
	assert.Zero(notifications)
}
