// Package eventstest restores actual pre-resource synthetic Events archives.
package eventstest

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	Require "github.com/stretchr/testify/require" //nolint:importas // Nested test scopes use assertion objects.
	"go.kenn.io/msgvault/internal/store"
)

// OpenLegacy restores a native pre-resource fixture and migrates its schema.
// ConfigureMCPEvents still owns resource adoption, so callers can inspect the
// retained delivery state before startup adopts its authority.
func OpenLegacy(t *testing.T, fixtures string) *store.Store {
	t.Helper()
	require := Require.New(t)
	destination := filepath.Join(t.TempDir(), "legacy.sqlite")
	fixture := "sqlite"
	if testURL := os.Getenv("MSGVAULT_TEST_DB"); strings.HasPrefix(testURL, "postgres://") || strings.HasPrefix(testURL, "postgresql://") {
		fixture = "postgres"
		parsed, err := url.Parse(testURL)
		require.NoError(err)
		admin, cleanup, err := store.OpenPostgresDB(testURL)
		require.NoError(err)
		t.Cleanup(cleanup)
		var suffix [12]byte
		_, err = rand.Read(suffix[:])
		require.NoError(err)
		name := "mcp_events_upgrade_" + hex.EncodeToString(suffix[:])
		_, err = admin.ExecContext(t.Context(), "CREATE DATABASE "+name+" TEMPLATE template0")
		require.NoError(err, "legacy PostgreSQL fixture requires an isolated test database")
		t.Cleanup(func() {
			_, err := admin.Exec("DROP DATABASE IF EXISTS " + name)
			Require.NoError(t, err, "drop only this test's database")
		})
		parsed.Path = "/" + name
		destination = parsed.String()
		// pg_dump's COPY, functions and psql directives must be restored by the
		// native client, never a handwritten statement splitter.
		psql, err := exec.LookPath("psql")
		require.NoError(err, "PostgreSQL migration fixtures require psql")
		input := readMCPEventsFixture(t, fixtures, fixture, "legacy.sql.gz")
		// #nosec G204 G702 -- Native psql is resolved from PATH; trusted test DSN is one argument, without a shell.
		command := exec.CommandContext(t.Context(), psql, "-X", "--no-psqlrc", "--set=ON_ERROR_STOP=1", "--dbname", destination)
		command.Stdin = strings.NewReader("DROP SCHEMA public;\n" + string(input))
		require.NoError(command.Run(), "restore synthetic legacy dump")
	} else {
		require.NoError(os.WriteFile(destination, readMCPEventsFixture(t, fixtures, fixture, "legacy.sqlite.gz"), 0600))
	}
	st, err := store.OpenForTest(destination)
	require.NoError(err)
	t.Cleanup(func() { Require.NoError(t, st.Close()) })
	// Current Store readers require the migrated columns. Runtime resource
	// adoption remains deferred until ConfigureMCPEvents.
	require.NoError(st.InitSchemaContext(t.Context()))
	return st
}

func readMCPEventsFixture(t *testing.T, fixtures, backend, name string) []byte {
	t.Helper()
	require := Require.New(t)
	file, err := os.Open(filepath.Join(fixtures, backend, name))
	require.NoError(err)
	t.Cleanup(func() { Require.NoError(t, file.Close()) })
	reader, err := gzip.NewReader(file)
	require.NoError(err)
	data, err := io.ReadAll(reader)
	require.NoError(err)
	require.NoError(reader.Close())
	return data
}
