package importer

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportEMLDirLogicalRecoveryBeforeFirstCheckpoint(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st, tmp := testutil.NewTestStore(t), t.TempDir()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-import", ImportNotifications: true})
	require.NoError(err)
	root := filepath.Join(tmp, "Synthetic.mailbox")
	require.NoError(os.MkdirAll(root, 0o700))
	require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <checkpoint-run@example.net>\r\nSubject: Synthetic checkpoint\r\n\r\nSynthetic body\r\n"), 0o600))
	// Reject the real first progress write. Durable policy must precede it,
	// and a later native invocation must recover without that checkpoint.
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION reject_synthetic_import_checkpoint() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.cursor_before IS NOT NULL AND (OLD.cursor_before IS NULL OR OLD.cursor_before='') THEN RAISE EXCEPTION 'synthetic checkpoint rejection'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_synthetic_import_checkpoint BEFORE UPDATE OF cursor_before ON sync_runs FOR EACH ROW EXECUTE FUNCTION reject_synthetic_import_checkpoint()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_synthetic_import_checkpoint BEFORE UPDATE OF cursor_before ON sync_runs WHEN NEW.cursor_before IS NOT NULL AND (OLD.cursor_before IS NULL OR OLD.cursor_before='') BEGIN SELECT RAISE(ABORT,'synthetic checkpoint rejection'); END`)
	}
	require.NoError(err)
	_, err = ImportEMLDir(t.Context(), st, root, EMLImportOptions{Identifier: "owner@example.net"})
	require.ErrorContains(err, "synthetic checkpoint rejection")
	// The native importer returns no summary when its first checkpoint fails.
	// Read only its synthetic source ID, without changing that error contract.
	var sourceID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM sources WHERE source_type='eml' AND identifier=?`), "owner@example.net").Scan(&sourceID))
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM import_runs WHERE source_id=?`), sourceID).Scan(&count))
	require.Equal(1, count, "native policy is durable before the first progress checkpoint")
	var firstID string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM import_runs WHERE source_id=?`), sourceID).Scan(&firstID))
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER reject_synthetic_import_checkpoint ON sync_runs`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER reject_synthetic_import_checkpoint`)
	}
	require.NoError(err)
	second, err := ImportEMLDir(t.Context(), st, root, EMLImportOptions{Identifier: "owner@example.net"})
	require.NoError(err)
	assert.True(second.WasResumed, "logical recovery does not require a progress checkpoint")
	var recoveredID string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT import_run_id FROM import_run_attempts WHERE sync_run_id=(SELECT MAX(id) FROM sync_runs WHERE source_id=?)`), sourceID).Scan(&recoveredID))
	assert.Equal(firstID, recoveredID)
	assert.Equal(int64(1), second.MessagesAdded)
}
