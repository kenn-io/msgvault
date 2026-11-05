package cmd

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestMeetingCountKeysRejectsFutureArchive(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.OpenForTest(path)
	require.NoError(err)
	require.NoError(st.InitSchema())
	_, err = st.DB().Exec(`UPDATE archive_metadata SET value = ? WHERE key = 'schema_version'`, strconv.Itoa(store.SchemaVersion+1))
	require.NoError(err)
	require.NoError(st.Close())
	_, err = readMeetingCountKeys(t.Context(), path)
	require.Error(err)
}
