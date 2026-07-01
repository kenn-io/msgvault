package cmd

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
)

func saveImportPstState(t *testing.T) func() {
	t.Helper()
	prevSourceType := importPstSourceType
	prevSkipFolders := importPstSkipFolders
	prevNoResume := importPstNoResume
	prevCheckpointInterval := importPstCheckpointInterval
	prevNoAttachments := importPstNoAttachments
	return func() {
		importPstSourceType = prevSourceType
		importPstSkipFolders = prevSkipFolders
		importPstNoResume = prevNoResume
		importPstCheckpointInterval = prevCheckpointInterval
		importPstNoAttachments = prevNoAttachments
	}
}

func TestImportPstRunsPostSourceMigrationForEligibleSourceTypes(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	markDaemonCLISubprocessForTest(t)

	tmp := t.TempDir()
	t.Cleanup(saveImportPstState(t))
	testCfg := lifecycleTestConfig(tmp)
	testCfg.Identity.Addresses = []string{"legacy@example.com"}
	withStoreResolverConfig(t, testCfg)

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(
		err, "open seed store")

	require.NoError(
		st.InitSchema(), "init seed schema")

	emailSource, err := st.GetOrCreateSource("gmail", "mailbox@example.com")
	require.NoError(
		err, "create eligible email source")

	require.NoError(
		st.Close(), "close seed store")

	pstPath, err := filepath.Abs("../../../internal/pst/testdata/support.pst")
	require.NoError(
		err, "pst fixture path")

	importPstSourceType = "mbox"
	importPstNoResume = true
	importPstCheckpointInterval = 200
	importPstNoAttachments = true

	var stdout bytes.Buffer
	cmd := &cobra.Command{Use: "import-pst"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&stdout)
	cmd.SetErr(io.Discard)

	err = importPstCmd.RunE(cmd, []string{"archive@example.com", pstPath})
	require.NoError(
		err, "import-pst")

	assert.Contains(stdout.String(), "Import complete.", "stdout")

	st, err = store.Open(testCfg.DatabaseDSN())
	require.NoError(
		err, "open store after import")

	t.Cleanup(func() { _ = st.Close() })

	emailIDs, err := st.ListAccountIdentities(emailSource.ID)
	require.NoError(
		err, "ListAccountIdentities gmail")

	require.Len(emailIDs, 1, "post-source migration should run for eligible email sources")
	assert.Equal("legacy@example.com", emailIDs[0].Address, "migrated identity address")

	pstSources, err := st.GetSourcesByIdentifier("archive@example.com")
	require.NoError(
		err, "get imported source")

	require.Len(pstSources, 1, "imported source")
	assert.Equal("mbox", pstSources[0].SourceType, "imported source type")
	pstIDs, err := st.ListAccountIdentities(pstSources[0].ID)
	require.NoError(
		err, "ListAccountIdentities imported source")

	require.Len(pstIDs, 2, "eligible imported source should keep default and migrated identities")
	assert.Equal("archive@example.com", pstIDs[0].Address, "default identity")
	assert.Equal("account-identifier", pstIDs[0].SourceSignal, "default identity signal")
	assert.Equal("legacy@example.com", pstIDs[1].Address, "migrated identity")
}

func TestRunPstPostImportMigrationsConfirmsDefaultIdentityBeforeHardErrorMigration(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	tmp := t.TempDir()
	testCfg := lifecycleTestConfig(tmp)
	testCfg.Identity.Addresses = []string{"legacy@example.com"}
	withStoreResolverConfig(t, testCfg)

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(
		err, "open store")

	t.Cleanup(func() { _ = st.Close() })
	require.NoError(
		st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("mbox", "archive@example.com")
	require.NoError(
		err, "create source")

	err = runPstPostImportMigrations(io.Discard, st, &importer.PstImportSummary{
		SourceID:   src.ID,
		HardErrors: true,
	}, "mbox", "archive@example.com")
	require.NoError(
		err, "post-import migrations")

	ids, err := st.ListAccountIdentities(src.ID)
	require.NoError(
		err, "ListAccountIdentities")

	require.Len(ids, 2, "hard-error migration should not suppress the source identifier")
	assert.Equal("archive@example.com", ids[0].Address, "default identity")
	assert.Equal("account-identifier", ids[0].SourceSignal, "default identity signal")
	assert.Equal("legacy@example.com", ids[1].Address, "migrated identity")
}
