package cmd

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gvoice"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportGvoiceStoresVoicemailAudioEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	previousBefore := importGvoiceBefore
	previousAfter := importGvoiceAfter
	previousLimit := importGvoiceLimit
	previousNoDefault := noDefaultIdentityImportGVoice
	previousCfg, previousLogger := cfg, logger
	previousCfgFile, previousHomeDir, previousVerbose := cfgFile, homeDir, verbose
	previousOut, previousErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	t.Cleanup(func() {
		importGvoiceBefore = previousBefore
		importGvoiceAfter = previousAfter
		importGvoiceLimit = previousLimit
		noDefaultIdentityImportGVoice = previousNoDefault
		cfg, logger = previousCfg, previousLogger
		cfgFile, homeDir, verbose = previousCfgFile, previousHomeDir, previousVerbose
		rootCmd.SetOut(previousOut)
		rootCmd.SetErr(previousErr)
		rootCmd.SetArgs(nil)
	})

	home := t.TempDir()
	voice := writeGvoiceCLIExport(t)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", home,
		"import-gvoice",
		"--no-default-identity",
		voice,
	})
	require.NoError(rootCmd.ExecuteContext(context.Background()))

	st, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })

	var stored, failed int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM attachments
		WHERE source_part_key = ? AND attachment_state = ?
	`), "gvoice:voicemail:audio", "stored").Scan(&stored))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM attachments
		WHERE source_part_key = ? AND attachment_state = ?
	`), "gvoice:voicemail:audio", "failed").Scan(&failed))
	assert.Equal(1, stored)
	assert.Equal(1, failed)

	var storagePath, contentHash string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT storage_path, content_hash FROM attachments
		WHERE source_part_key = ? AND attachment_state = ?
	`), "gvoice:voicemail:audio", "stored").Scan(&storagePath, &contentHash))
	got, err := os.ReadFile(filepath.Join(home, "attachments", filepath.FromSlash(storagePath)))
	require.NoError(err)
	assert.Equal([]byte("cli voicemail bytes"), got)
	assert.Equal(path.Join(contentHash[:2], contentHash), storagePath)

	var filename, mimeType, skipReason sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT filename, mime_type, attachment_skip_reason FROM attachments
		WHERE source_part_key = ? AND attachment_state = ?
	`), "gvoice:voicemail:audio", "failed").Scan(&filename, &mimeType, &skipReason))
	assert.Empty(filename.String)
	assert.Empty(mimeType.String)
	assert.Equal("fetch_failure", skipReason.String)

	updated := []byte("updated cli voicemail bytes")
	require.NoError(os.WriteFile(
		filepath.Join(voice, "Calls", "Test User - Voicemail - 2024-01-02T03_04_05Z.mp3"),
		updated,
		0o600,
	))
	// An interrupted subprocess can persist the import before CLI finalization.
	// Retrying identical input must still refresh the published attachment data.
	client, err := gvoice.NewClient(voice, gvoice.WithAttachmentsDir(cfg.AttachmentsDir()))
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	source, err := st.GetOrCreateSource("google_voice", client.Identifier())
	require.NoError(err)
	_, err = client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	rootCmd.SetArgs([]string{
		"--home", home,
		"import-gvoice",
		"--no-default-identity",
		voice,
	})
	require.NoError(rootCmd.ExecuteContext(context.Background()))

	engine, err := query.NewDuckDBEngine(cfg.AnalyticsDir(), "", nil)
	require.NoError(err)
	result, queryErr := engine.QuerySQL(context.Background(), `
		SELECT size FROM attachments
		WHERE filename = 'Test User - Voicemail - 2024-01-02T03_04_05Z.mp3'
	`)
	closeErr := engine.Close()
	require.NoError(queryErr)
	require.NoError(closeErr)
	require.Len(result.Rows, 1)
	assert.EqualValues(len(updated), result.Rows[0][0])

	var revisionBefore, revisionAfter int64
	require.NoError(st.DB().QueryRow(
		`SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = 'derived_data_revision'), 0)`,
	).Scan(&revisionBefore))
	require.NoError(rootCmd.ExecuteContext(context.Background()))
	require.NoError(st.DB().QueryRow(
		`SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = 'derived_data_revision'), 0)`,
	).Scan(&revisionAfter))
	assert.Equal(revisionBefore, revisionAfter)
}

func writeGvoiceCLIExport(t *testing.T) string {
	t.Helper()
	require := require.New(t)
	fixtureDir, err := filepath.Abs("../../../internal/gvoice/testdata/voicemail")
	require.NoError(err)
	voice := t.TempDir()
	calls := filepath.Join(voice, "Calls")
	require.NoError(os.MkdirAll(calls, 0o700))
	phones, err := os.ReadFile(filepath.Join(fixtureDir, "phones.vcf"))
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(voice, "Phones.vcf"), phones, 0o600))

	withAudio, err := os.ReadFile(filepath.Join(fixtureDir, "with-audio.html"))
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(calls, "Test User - Voicemail - 2024-01-02T03_04_05Z.html"), withAudio, 0o600))
	require.NoError(os.WriteFile(filepath.Join(calls, "Test User - Voicemail - 2024-01-02T03_04_05Z.mp3"), []byte("cli voicemail bytes"), 0o600))

	noAudio, err := os.ReadFile(filepath.Join(fixtureDir, "no-audio.html"))
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(calls, "Test User - Voicemail - 2024-01-04T03_04_05Z.html"), noAudio, 0o600))
	return voice
}
