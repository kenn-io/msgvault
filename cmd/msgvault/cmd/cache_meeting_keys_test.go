package cmd

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestMeetingCountKeysRejectsFutureArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.OpenForTest(path)
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
	_, err = st.DB().Exec(`UPDATE archive_metadata SET value = ? WHERE key = 'schema_version'`, strconv.Itoa(store.SchemaVersion+1))
	require.NoError(t, err)
	require.NoError(t, st.Close())
	_, err = readMeetingCountKeys(t.Context(), path)
	require.Error(t, err)
}
