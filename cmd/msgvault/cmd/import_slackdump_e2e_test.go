package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportSlackdumpEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	markDaemonCLISubprocessForTest(t)
	t.Cleanup(saveMessengerState(t))
	resetImportSlackdumpFlagsAfterTest(t)

	home := t.TempDir()
	fixture, err := filepath.Abs("../../../internal/slack/testdata/slackdump/standard")
	require.NoError(err)

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", home,
		"import-slackdump",
		"--me", "alice@example.com",
		fixture,
	})
	require.NoError(rootCmd.ExecuteContext(context.Background()))
	assert.Contains(stdout.String(), "Import complete!")
	assert.Contains(stdout.String(), "Messages:      6 processed, 6 added, 0 updated")
	assert.Contains(stdout.String(), "Attachments:   2 stored, 1 missing, 0 skipped")

	st, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })

	sources, err := st.GetSourcesByIdentifier("T_TEST:UALICE")
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal("slackdump", sources[0].SourceType)
	identities, err := st.ListAccountIdentities(sources[0].ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("T_TEST:UALICE", identities[0].Address)
	assert.Equal("account-identifier", identities[0].SourceSignal)
	assert.Contains(stdout.String(), "Confirmed identity T_TEST:UALICE")
	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = duckdb.Close() })
	var ownerRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE source_id = ?`,
		filepath.Join(home, "analytics", "owner_participants", "*.parquet"), sources[0].ID,
	).Scan(&ownerRows))
	assert.Equal(1, ownerRows)

	results, total, err := st.SearchMessages("first day", 0, 10)
	require.NoError(err)
	assert.EqualValues(1, total)
	require.Len(results, 1)
	assert.Equal("C001:1704067200.000001", results[0].SourceMessageID)

	var linked int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM messages child
		JOIN messages parent ON parent.id = child.reply_to_message_id
		WHERE child.source_message_id = 'C001:1704153600.000002'
		  AND parent.source_message_id = 'C001:1704067200.000001'`).Scan(&linked))
	assert.Equal(1, linked)

	var storagePath string
	require.NoError(st.DB().QueryRow(`
		SELECT storage_path FROM attachments
		WHERE source_attachment_id = 'slack:F_STD' AND attachment_state = 'stored'`).Scan(&storagePath))
	content, err := os.ReadFile(filepath.Join(home, "attachments", filepath.FromSlash(storagePath)))
	require.NoError(err)
	assert.Equal([]byte("standard attachment bytes\n"), content)
}

func TestImportSlackdumpNoDefaultIdentity(t *testing.T) {
	require := require.New(t)

	markDaemonCLISubprocessForTest(t)
	t.Cleanup(saveMessengerState(t))
	resetImportSlackdumpFlagsAfterTest(t)

	home := t.TempDir()
	fixture, err := filepath.Abs("../../../internal/slack/testdata/slackdump/standard")
	require.NoError(err)

	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", home,
		"import-slackdump",
		"--me", "UALICE",
		"--no-default-identity",
		fixture,
	})
	require.NoError(rootCmd.ExecuteContext(context.Background()))

	st, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	sources, err := st.GetSourcesByIdentifier("T_TEST:UALICE")
	require.NoError(err)
	require.Len(sources, 1)
	identities, err := st.ListAccountIdentities(sources[0].ID)
	require.NoError(err)
	assert.Empty(t, identities)
}

func TestImportSlackdumpPartialFailureConfirmsIdentity(t *testing.T) {
	require := require.New(t)

	markDaemonCLISubprocessForTest(t)
	t.Cleanup(saveMessengerState(t))
	resetImportSlackdumpFlagsAfterTest(t)

	home := t.TempDir()
	fixture, err := filepath.Abs("../../../internal/slack/testdata/slackdump/standard")
	require.NoError(err)
	exportRoot := filepath.Join(t.TempDir(), "export")
	require.NoError(os.CopyFS(exportRoot, os.DirFS(fixture)))
	require.NoError(os.WriteFile(
		filepath.Join(exportRoot, "general", "2024-01-02.json"),
		[]byte(`[{"type":"message"`),
		0o600,
	))

	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", home,
		"import-slackdump",
		"--me", "UALICE",
		exportRoot,
	})
	err = rootCmd.ExecuteContext(context.Background())
	require.Error(err)
	require.ErrorContains(err, "2024-01-02.json")

	st, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	var messages int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Positive(t, messages)
	sources, err := st.GetSourcesByIdentifier("T_TEST:UALICE")
	require.NoError(err)
	require.Len(sources, 1)
	identities, err := st.ListAccountIdentities(sources[0].ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal(t, "T_TEST:UALICE", identities[0].Address)
}

func resetImportSlackdumpFlagsAfterTest(t *testing.T) {
	t.Helper()
	command, _, err := rootCmd.Find([]string{"import-slackdump"})
	require.NoError(t, err)
	t.Cleanup(func() {
		command.Flags().VisitAll(func(flag *pflag.Flag) {
			require.NoError(t, flag.Value.Set(flag.DefValue))
			flag.Changed = false
		})
	})
}
