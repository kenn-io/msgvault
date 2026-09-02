package store

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenForTestCreatesFileURIParentAndPreservesQuery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "msgvault.db")
	uriPath := filepath.ToSlash(dbPath)
	if filepath.VolumeName(dbPath) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     uriPath,
		RawQuery: "cache=shared",
	}).String()

	st, err := OpenForTest(dsn)
	require.NoError(t, err, "open SQLite file URI")
	require.NoError(t, st.Close(), "close SQLite file URI")
	_, err = os.Stat(dbPath)
	require.NoError(t, err, "stat SQLite database created from file URI")
}

func TestOpenReadOnlyAcceptsFileURIWithExistingQuery(t *testing.T) {
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
	writable, err := OpenForTest(dbPath)
	require.NoError(err, "open seed database")
	require.NoError(writable.InitSchema(), "initialize seed database")
	require.NoError(writable.Close(), "close seed database")

	uriPath := filepath.ToSlash(dbPath)
	if filepath.VolumeName(dbPath) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     uriPath,
		RawQuery: "cache=shared",
	}).String()

	readOnly, err := OpenReadOnly(dsn)
	require.NoError(err, "open SQLite file URI read-only")
	require.NoError(readOnly.Close(), "close SQLite file URI read-only")
}
