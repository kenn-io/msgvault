package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

const whatsappIdentityTestPhone = "+15555550100"

func TestImportWhatsAppConfirmsDefaultIdentityWithRecoverableErrors(t *testing.T) {
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, true, false)
	output, err := runWhatsAppIdentityCommand(t, home, fixture, false)

	require.NoError(t, err)
	assert.Contains(t, output, "Confirmed identity "+whatsappIdentityTestPhone)
	assert.Contains(t, output, "phone-e164")
	assert.Contains(t, output, "Messages:       3 processed, 1 added, 2 skipped")
	assert.Contains(t, output, "Errors:         1")

	identities := whatsappIdentityTestIdentities(t, home)
	require.Len(t, identities, 1)
	assert.Equal(t, whatsappIdentityTestPhone, identities[0].Address)
	assert.Equal(t, "phone-e164", identities[0].SourceSignal)
}

func TestImportWhatsAppConfirmsDefaultIdentityOnCleanImport(t *testing.T) {
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, false, false)
	_, err := runWhatsAppIdentityCommand(t, home, fixture, false)

	require.NoError(t, err)
	assert.Len(t, whatsappIdentityTestIdentities(t, home), 1)
}

func TestImportWhatsAppDoesNotConfirmIdentityWithNoDefaultIdentity(t *testing.T) {
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, true, false)
	_, err := runWhatsAppIdentityCommand(t, home, fixture, true)

	require.NoError(t, err)
	assert.Empty(t, whatsappIdentityTestIdentities(t, home))
	assert.Len(t, whatsappIdentityTestSources(t, home), 1)
}

func TestImportWhatsAppDoesNotConfirmIdentityOnHardError(t *testing.T) {
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, false, true)
	_, err := runWhatsAppIdentityCommand(t, home, fixture, false)

	require.ErrorContains(t, err, "import failed")
	assert.Empty(t, whatsappIdentityTestIdentities(t, home))
	assert.Len(t, whatsappIdentityTestSources(t, home), 1)
}

func TestImportWhatsAppDoesNotCreateSourceForUnknownDatabase(t *testing.T) {
	home := t.TempDir()
	fixture := createWhatsAppUnknownDatabaseFixture(t)
	_, err := runWhatsAppIdentityCommand(t, home, fixture, false)

	require.ErrorContains(t, err, "import failed")
	assert.Empty(t, whatsappIdentityTestSources(t, home))
	assert.Empty(t, whatsappIdentityTestIdentities(t, home))
}

func TestImportWhatsAppIdentityIsIdempotent(t *testing.T) {
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, true, false)

	_, err := runWhatsAppIdentityCommand(t, home, fixture, false)
	require.NoError(t, err)
	_, err = runWhatsAppIdentityCommand(t, home, fixture, false)
	require.NoError(t, err)

	identities := whatsappIdentityTestIdentities(t, home)
	require.Len(t, identities, 1)
	assert.Equal(t, whatsappIdentityTestPhone, identities[0].Address)
}

func runWhatsAppIdentityCommand(
	t *testing.T,
	home, fixture string,
	noDefaultIdentity bool,
) (string, error) {
	t.Helper()

	oldCfg := cfg
	oldNoDefaultIdentity := noDefaultIdentityImportWhatsApp
	oldImportPhone := importPhone
	oldImportMediaDir := importMediaDir
	oldImportContacts := importContacts
	oldImportLimit := importLimit
	oldImportDisplayName := importDisplayName
	t.Cleanup(func() {
		cfg = oldCfg
		noDefaultIdentityImportWhatsApp = oldNoDefaultIdentity
		importPhone = oldImportPhone
		importMediaDir = oldImportMediaDir
		importContacts = oldImportContacts
		importLimit = oldImportLimit
		importDisplayName = oldImportDisplayName
	})

	withStoreResolverConfig(t, &config.Config{
		HomeDir: home,
		Data: config.DataConfig{
			DataDir: home,
		},
		Analytics: config.AnalyticsConfig{
			Engine:         config.AnalyticsEngineAuto,
			AutoBuildCache: true,
		},
	})
	noDefaultIdentityImportWhatsApp = noDefaultIdentity
	importPhone = whatsappIdentityTestPhone
	importMediaDir = ""
	importContacts = ""
	importLimit = 0
	importDisplayName = ""

	var output strings.Builder
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	oldStdout := os.Stdout
	stdoutReader, stdoutWriter, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = stdoutWriter
	defer func() {
		os.Stdout = oldStdout
		_ = stdoutWriter.Close()
		_ = stdoutReader.Close()
	}()
	runErr := runWhatsAppImport(cmd, fixture)
	require.NoError(t, stdoutWriter.Close())
	os.Stdout = oldStdout
	var printed bytes.Buffer
	_, copyErr := io.Copy(&printed, stdoutReader)
	require.NoError(t, copyErr)
	require.NoError(t, stdoutReader.Close())
	return output.String() + printed.String(), runErr
}

func createWhatsAppIdentityFixture(t *testing.T, recoverable, hardError bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)

	_, err = db.Exec(`
		CREATE TABLE ZWACHATSESSION (
			Z_PK INTEGER PRIMARY KEY,
			ZCONTACTJID TEXT,
			ZPARTNERNAME TEXT,
			ZSESSIONTYPE INTEGER,
			ZLASTMESSAGEDATE TIMESTAMP
		);
		CREATE TABLE ZWAMESSAGE (
			Z_PK INTEGER PRIMARY KEY,
			ZCHATSESSION INTEGER,
			ZGROUPMEMBER INTEGER,
			ZSTANZAID TEXT,
			ZISFROMME INTEGER,
			ZMESSAGEDATE TIMESTAMP,
			ZTEXT TEXT,
			ZMESSAGETYPE INTEGER,
			ZFROMJID TEXT
		);
		INSERT INTO ZWACHATSESSION VALUES
			(1, '15555550101@s.whatsapp.net', 'Test Contact', 0, 700000003);
		INSERT INTO ZWAMESSAGE VALUES
			(1, 1, NULL, 'unique-message', 0, 700000000, 'imported text', 0,
			 '15555550101@s.whatsapp.net');
	`)
	require.NoError(t, err)
	if !hardError {
		_, err = db.Exec(`
			CREATE TABLE ZWAGROUPMEMBER (
				Z_PK INTEGER PRIMARY KEY,
				ZCHATSESSION INTEGER,
				ZMEMBERJID TEXT,
				ZCONTACTNAME TEXT,
				ZFIRSTNAME TEXT,
				ZISADMIN INTEGER
			);
		`)
		require.NoError(t, err)
		if recoverable {
			_, err = db.Exec(`
				INSERT INTO ZWAMESSAGE VALUES
					(2, 1, NULL, 'duplicate-message', 0, 700000001, 'duplicate one', 0,
					 '15555550101@s.whatsapp.net'),
					(3, 1, NULL, 'duplicate-message', 0, 700000002, 'duplicate two', 0,
					 '15555550101@s.whatsapp.net');
			`)
			require.NoError(t, err)
		}
	}
	require.NoError(t, db.Close())
	return path
}

func createWhatsAppUnknownDatabaseFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unknown.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE unrelated (id INTEGER PRIMARY KEY)")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return path
}

func whatsappIdentityTestSources(t *testing.T, home string) []*store.Source {
	t.Helper()
	db, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	sources, err := db.GetSourcesByIdentifier(whatsappIdentityTestPhone)
	require.NoError(t, err)
	return sources
}

func whatsappIdentityTestIdentities(t *testing.T, home string) []store.AccountIdentity {
	t.Helper()
	db, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	sources, err := db.GetSourcesByIdentifier(whatsappIdentityTestPhone)
	require.NoError(t, err)
	if len(sources) == 0 {
		return nil
	}
	identities, err := db.ListAccountIdentities(sources[0].ID)
	require.NoError(t, err)
	return identities
}
