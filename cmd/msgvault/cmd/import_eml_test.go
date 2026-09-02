package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportEMLCommandRequiresIdentifier(t *testing.T) {
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cmd := newImportEMLCommand()
	cmd.SetArgs([]string{t.TempDir()})

	err := cmd.Execute()

	assert.ErrorContains(t, err, "--identifier is required")
}

func TestImportEMLCommandRequiresMailboxRoot(t *testing.T) {
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cmd := newImportEMLCommand()
	cmd.SetArgs([]string{"--identifier", "alice@example.com"})

	err := cmd.Execute()

	assert.ErrorContains(t, err, "requires exactly 1 arg")
}

func TestImportEMLCommandRefreshesCacheAfterPostMigrationFailure(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	testCfg := lifecycleTestConfig(dataDir)
	testCfg.Identity.Addresses = []string{"legacy@example.com"}
	withStoreResolverConfig(t, testCfg)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	_, err = st.DB().Exec(`
		CREATE TRIGGER fail_legacy_identity
		BEFORE INSERT ON account_identities
		WHEN NEW.source_signal = 'config_migration'
		BEGIN
			SELECT RAISE(ABORT, 'forced legacy identity migration failure');
		END`)
	require.NoError(err)
	require.NoError(st.Close())

	mailbox := filepath.Join(dataDir, "MailMate", "Inbox.mailbox")
	require.NoError(os.MkdirAll(mailbox, 0o700))
	require.NoError(os.WriteFile(
		filepath.Join(mailbox, "message.eml"),
		[]byte("From: Alice <alice@example.com>\r\nSubject: test\r\n\r\nbody"),
		0o600,
	))

	cacheSentinel := errors.New("EML cache refresh sentinel")
	buildCacheBeforeMessagesExportHook = func() error { return cacheSentinel }
	t.Cleanup(func() { buildCacheBeforeMessagesExportHook = nil })

	cmd := newImportEMLCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"--identifier", "archive@example.com",
		filepath.Join(dataDir, "MailMate"),
	})
	err = cmd.Execute()

	require.ErrorContains(err, "forced legacy identity migration failure")
	require.ErrorIs(err, cacheSentinel,
		"cache refresh must still run after post-import migrations fail")
}

func TestRunEMLPostImportMigrationsConfirmsIdentityAfterHardErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	testCfg := lifecycleTestConfig(t.TempDir())
	testCfg.Identity.Addresses = []string{"legacy@example.com"}
	withStoreResolverConfig(t, testCfg)

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "init schema")
	src, err := st.GetOrCreateSource("eml", "archive@example.com")
	require.NoError(err, "create source")

	err = runEMLPostImportMigrations(io.Discard, st, &importer.EMLImportSummary{
		SourceID:   src.ID,
		HardErrors: true,
	}, importEMLFlags{
		identifier: "archive@example.com",
		sourceType: "eml",
	})
	require.NoError(err, "post-import migrations")

	identities, err := st.ListAccountIdentities(src.ID)
	require.NoError(err, "list account identities")
	require.Len(identities, 2, "hard-error migration must retain the source identity")
	assert.Equal("archive@example.com", identities[0].Address, "default identity")
	assert.Equal("account-identifier", identities[0].SourceSignal, "default identity signal")
	assert.Equal("legacy@example.com", identities[1].Address, "migrated identity")
}

func TestRunEMLPostImportMigrationsSkipsDefaultIdentityForNonEmailSource(t *testing.T) {
	require := require.New(t)
	testCfg := lifecycleTestConfig(t.TempDir())
	withStoreResolverConfig(t, testCfg)

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "init schema")
	src, err := st.GetOrCreateSource("whatsapp", "+15550001111")
	require.NoError(err, "create source")

	err = runEMLPostImportMigrations(io.Discard, st, &importer.EMLImportSummary{
		SourceID: src.ID,
	}, importEMLFlags{
		identifier: "+15550001111",
		sourceType: "whatsapp",
	})
	require.NoError(err, "post-import migrations")

	identities, err := st.ListAccountIdentities(src.ID)
	require.NoError(err, "list account identities")
	assert.Empty(t, identities, "non-email source must not gain an email identity")
}
