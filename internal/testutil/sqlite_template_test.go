package testutil

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestNewSQLiteTestStoreAt(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st := NewSQLiteTestStoreAt(t, path)
	source, err := st.GetOrCreateSource("gmail", "clone@example.com")
	require.NoError(err)
	require.NoError(st.Close())

	reopened, err := store.Open(path)
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	sources, err := reopened.ListSources("")
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(t, source.ID, sources[0].ID)
}

func TestNewSQLiteTestStoreAtToleratesEarlyClose(t *testing.T) {
	var logged bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	t.Run("closes before cleanup", func(t *testing.T) {
		st := NewSQLiteTestStoreAt(t, filepath.Join(t.TempDir(), "archive.db"))
		require.NoError(t, st.Close())
	})

	assert.NotContains(t, logged.String(), "maintenance failed")
}

// TestSQLiteFixtureClonesTemplateWithoutReplayingDDL pins the fixture's cost
// model: the schema is built once per test binary and every later fixture is a
// copy of that file. A fixture that replayed the DDL would show CREATE
// statements in the SQL trace captured here.
func TestSQLiteFixtureClonesTemplateWithoutReplayingDDL(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	NewSQLiteTestStore(t) // first fixture in the binary builds the template

	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true, MaxStmtChars: 10_000})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	var logged bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	st := NewSQLiteTestStore(t)

	var integrity string
	require.NoError(st.DB().QueryRow("PRAGMA integrity_check").Scan(&integrity), "integrity check")
	assert.Equal("ok", integrity, "cloned database is intact")
	assert.Equal(0, countSources(t, st), "cloned fixture starts empty")

	_, err := st.GetOrCreateSource("gmail", "clone@example.com")
	require.NoError(err, "write through the cloned fixture")
	assert.Equal(1, countSources(t, st), "cloned fixture is writable")

	trace := strings.ToLower(logged.String())
	assert.NotContains(trace, "create table", "fixture must clone the template, not replay the schema")
}
