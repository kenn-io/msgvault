//go:build fts5

package granola

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestImport_FTSIndexed verifies imported meetings are searchable: subject,
// summary prose, and transcript text must all hit via a direct FTS5 MATCH.
// Gated on the fts5 build tag (the project's canonical test invocation).
func TestImport_FTSIndexed(t *testing.T) {
	require := require.New(t)
	testutil.SkipIfPostgres(t, "directly MATCH-queries the SQLite FTS5 vtable; PG uses a tsvector column")

	api := &fakeAPI{notes: map[string][]byte{
		"not_Ab12Cd34Ef56Gh": loadFixture(t, "note_full.json"),
	}}
	imp, st := newTestImporter(t, api)
	require.True(st.FTS5Available(), "FTS5 build tag set but FTS5 not available")

	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)

	for _, term := range []string{"quarterly", "priorities", "budget"} {
		var hits int
		require.NoError(st.DB().QueryRow(
			`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, term).Scan(&hits),
			"MATCH %q", term)
		require.Equal(1, hits, "expected an FTS hit for %q", term)
	}
}
