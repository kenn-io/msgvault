package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetadataTrigramSubsetCopy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := metadataTrigramStore(t)
	require.NoError(st.InitSchema())
	destination := filepath.Join(t.TempDir(), "subset")
	_, err := CopySubset(st.dbPath, destination, 10, false)
	require.NoError(err)
	copied, err := OpenForTest(filepath.Join(destination, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(copied.Close()) })
	assert.Equal(1, metadataTrigramCount(t, copied, "messages_metadata_fts", "eedle"))
	assert.Equal(1, metadataTrigramCount(t, copied, "participants_metadata_fts", "école"))
	assert.Equal(1, metadataTrigramCount(t, copied, "recipients_metadata_fts", "rrence"))
}
