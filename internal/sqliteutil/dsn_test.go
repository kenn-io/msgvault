package sqliteutil

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveDSNNormalizesNetURLWindowsPath(t *testing.T) {
	dsn := `file://C:%5CUsers%5Crunner%5Carchive.db?cache=shared`

	normalized, path, err := ResolveDSN(dsn)
	require.NoError(t, err, "ResolveDSN")
	assert.Equal(t, `C:\Users\runner\archive.db`, path)
	assert.Equal(t, `file:///C:/Users/runner/archive.db?cache=shared`, normalized)
}

func TestResolveDSNTreatsLocalhostAuthorityAsLocal(t *testing.T) {
	dsn := `file://localhost/var/lib/msgvault/archive.db?cache=shared`

	normalized, path, err := ResolveDSN(dsn)
	require.NoError(t, err, "ResolveDSN")
	assert.Equal(t, filepath.FromSlash(`/var/lib/msgvault/archive.db`), path)
	assert.Equal(t, dsn, normalized)
}
