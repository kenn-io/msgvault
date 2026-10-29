//go:build fts5

package telegram

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestImportFlattenedBodyFTSIndexed verifies flattened message text lands in
// FTS5, so Telegram messages are searchable like every other text source.
// Gated on the fts5 build tag like the fbmessenger FTS assertions.
func TestImportFlattenedBodyFTSIndexed(t *testing.T) {
	testutil.SkipIfPostgres(t, "directly MATCH-queries the SQLite FTS5 vtable")
	st := testutil.NewTestStore(t)
	dir, err := filepath.Abs(filepath.Join("testdata", "full"))
	require.NoError(t, err)
	_, err = Import(context.Background(), st, ImportOptions{
		RootDir: dir, AttachmentsDir: t.TempDir(),
	})
	require.NoError(t, err)
	require.True(t, st.FTS5Available(), "FTS5 build tag set but FTS5 not available")

	var count int
	require.NoError(t, st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, `"example.org"`,
	).Scan(&count))
	assert.GreaterOrEqual(t, count, 1, "entity link text is searchable")

	var senders int
	require.NoError(t, st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, `"Sample"`,
	).Scan(&senders))
	assert.GreaterOrEqual(t, senders, 1, "sender names are searchable")
}
