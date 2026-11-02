package cmd

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestResolveSignalInput(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "database.sqlite")
	require.NoError(os.WriteFile(dbPath, []byte("db"), 0o600))
	otherFiles := t.TempDir()

	gotDB, gotFiles, err := resolveSignalInput(dir, "")
	require.NoError(err)
	assert.Equal(dbPath, gotDB)
	assert.Equal(dir, gotFiles)

	gotDB, gotFiles, err = resolveSignalInput(dbPath, otherFiles)
	require.NoError(err)
	assert.Equal(dbPath, gotDB)
	assert.Equal(otherFiles, gotFiles)

	_, _, err = resolveSignalInput(t.TempDir(), "")
	require.Error(err)
	assert.Contains(err.Error(), "no database.sqlite")

	_, _, err = resolveSignalInput(dbPath, filepath.Join(dir, "missing"))
	require.Error(err)
	assert.Contains(err.Error(), "attachments directory not found")
}

// writeSignalCommandFixture writes a minimal Signal Android database: one
// direct chat with a received and a sent message.
func writeSignalCommandFixture(t *testing.T) string {
	t.Helper()
	require := require.New(t)
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "database.sqlite"))
	require.NoError(err)
	defer func() { require.NoError(db.Close()) }()
	_, err = db.Exec(`
		CREATE TABLE recipient (_id INTEGER PRIMARY KEY, e164 TEXT, aci TEXT, group_id TEXT, profile_joined_name TEXT);
		CREATE TABLE thread (_id INTEGER PRIMARY KEY, recipient_id INTEGER);
		CREATE TABLE message (_id INTEGER PRIMARY KEY, date_sent INTEGER, date_received INTEGER,
			thread_id INTEGER, from_recipient_id INTEGER, to_recipient_id INTEGER, type INTEGER, body TEXT,
			quote_id INTEGER DEFAULT 0, quote_author INTEGER DEFAULT 0,
			latest_revision_id INTEGER, original_message_id INTEGER);
		INSERT INTO recipient VALUES
			(1, '+15555550100', '0f0f0f0f-0000-4000-8000-000000000001', NULL, 'Owner'),
			(2, '+15555550101', '0f0f0f0f-0000-4000-8000-000000000002', NULL, 'Alice Example');
		INSERT INTO thread VALUES (1, 2);
		INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body)
		VALUES (1, 1700000000000, 1700000000001, 1, 2, 1, 10485780, 'hello'),
		       (2, 1700000060000, 1700000060001, 1, 1, 2, 10485783, 'hi');
	`)
	require.NoError(err)
	return dir
}

func TestImportSignalConfirmsNormalizedPhone(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	markDaemonCLISubprocessForTest(t)
	home := t.TempDir()
	cfg := config.NewDefaultConfig()
	testCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	cfg.HomeDir = home
	cfg.Data.DataDir = home

	command := &cobra.Command{Use: "test"}
	command.SetContext(testCtx)
	command.AddCommand(newImportSignalCmd())
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"import-signal", writeSignalCommandFixture(t), "--phone", "+1 555 555 0100"})
	require.NoError(command.Execute())

	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	source, err := st.GetOrCreateSource("signal", "+15555550100")
	require.NoError(err)
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("+15555550100", identities[0].Address)
}
