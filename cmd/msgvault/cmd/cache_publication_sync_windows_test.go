//go:build windows

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindowsCachePublicationDurabilityHelpers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "journal.json")
	want := []byte(`{"phase":"prepared"}`)

	require.NoError(t, writeDurableFile(path, want, 0o600))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	require.NoError(t, syncFile(path))
	require.NoError(t, syncDirectory(root))
}
