//go:build sqlite_vec

package sqlitevec

import (
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExistingOnlyCleanupOpenNeverCreatesFile(t *testing.T) {
	for _, uri := range []bool{false, true} {
		name := "path"
		if uri {
			name = "file_uri"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vector store%.db")
			dsn := path
			if uri {
				dsn = (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc"}).String()
			}
			_, err := Open(t.Context(), Options{Path: dsn, ExistingOnly: true})
			require.Error(t, err)
			assert.NoFileExists(t, path)
			initial, err := Open(t.Context(), Options{Path: path})
			require.NoError(t, err)
			require.NoError(t, initial.Close())
			cleanup, err := Open(t.Context(), Options{Path: dsn, ExistingOnly: true, ReadOnly: true})
			require.NoError(t, err)
			defer func() { require.NoError(t, cleanup.Close()) }()
			_, err = cleanup.CreateGeneration(t.Context(), "synthetic", 3, "")
			require.NoError(t, err, "existing-only cleanup handle remains writable")
		})
	}
}
