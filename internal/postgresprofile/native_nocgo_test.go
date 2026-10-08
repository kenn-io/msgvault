//go:build !cgo

package postgresprofile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/store"
)

func TestNativeBackendsFailBeforeCreatingFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	db, err := store.OpenContext(t.Context(), filepath.Join(root, "sqlite", "archive.db"))
	require.Error(err)
	assert.Nil(db)
	analytics, err := duckdbutil.Open(t.Context(), duckdbutil.InteractivePolicy(filepath.Join(root, "duckdb")))
	require.Error(err)
	assert.Nil(analytics)
	entries, err := os.ReadDir(root)
	require.NoError(err)
	assert.Empty(entries)
}
