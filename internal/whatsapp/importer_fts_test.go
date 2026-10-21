//go:build fts5

package whatsapp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportReportsFTSWriteErrors(t *testing.T) {
	waDBPath := createGroupParticipantsImportFixture(t, nil)
	progress := &importProgressRecorder{}
	st := testutil.NewSQLiteTestStore(t)
	require.True(t, st.FTS5Available(), "fts5 build tag set but FTS5 unavailable")
	_, err := st.DB().Exec(`DROP TABLE messages_fts`)
	require.NoError(t, err)

	opts := DefaultOptions()
	opts.Phone = "+12025550123"
	opts.DisplayName = "Example User"

	summary, err := NewImporter(st, progress).Import(context.Background(), waDBPath, opts)
	require.NoError(t, err)
	assert.Equal(t, int64(2), summary.MessagesAdded)
	assert.Equal(t, int64(2), summary.Errors)
	assert.Len(t, progress.errors, 2)
	for _, progressErr := range progress.errors {
		assert.Contains(t, progressErr, "upsert FTS:")
		assert.Contains(t, progressErr, "messages_fts")
	}
}
