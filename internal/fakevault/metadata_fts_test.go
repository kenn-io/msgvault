package fakevault_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateMaintainsMetadataTrigrams(t *testing.T) {
	dir := t.TempDir()
	generate(t, dir, 12, 0, 7, false)
	generate(t, dir, 8, 0, 7, true)
	db := openVaultDB(t, dir)
	for table, index := range map[string]string{"messages": "messages_metadata_fts", "participants": "participants_metadata_fts", "message_recipients": "recipients_metadata_fts"} {
		var want, got int
		require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&want))
		require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM "+index).Scan(&got))
		assert.Equal(t, want, got, table+" stays indexed during generation and append")
	}
}
