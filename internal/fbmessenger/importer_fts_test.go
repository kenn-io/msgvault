//go:build fts5

package fbmessenger

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestImportDYI_MojibakeFTSIndexed verifies that mojibake-repaired body
// text (e.g. "café") lands in message_bodies AND is indexed by FTS5 so a
// direct MATCH query returns a hit. Gated on the fts5 build tag so the
// FTS assertion is always active under the project's canonical
// `go test -tags fts5 ./...` invocation.
func TestImportDYI_MojibakeFTSIndexed(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	testutil.SkipIfPostgres(t, "directly MATCH-queries the SQLite FTS5 vtable; PG uses a tsvector column exercised via FTSSearchClause")
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	require.True(st.FTS5Available(), "FTS5 build tag set but FTS5 not available in this binary")

	// The body stored in message_bodies must contain literal "café".
	var body string
	err := st.DB().QueryRow(
		`SELECT body_text FROM message_bodies WHERE body_text LIKE '%café%'`,
	).Scan(&body)
	require.NoError(err, "body query")
	assert.Contains(body, "café")

	var count int
	err = st.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?", "café",
	).Scan(&count)
	require.NoError(err, "fts query")
	assert.GreaterOrEqual(count, 1, "fts match for café")
}

// TestImportDYI_ReactionsDualPath verifies that reactions land both as
// first-class rows in the reactions table and as an appended
// "[reacted: ...]" suffix in body_text that FTS5 can match. Gated on
// the fts5 build tag; the FTS MATCH assertion is unconditional.
func TestImportDYI_ReactionsDualPath(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	testutil.SkipIfPostgres(t, "directly MATCH-queries the SQLite FTS5 vtable; PG uses a tsvector column exercised via FTSSearchClause")
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	require.True(st.FTS5Available(), "FTS5 build tag set but FTS5 not available in this binary")

	// Count reactions on the message that contains café.
	var n int
	err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM reactions r
		JOIN message_bodies b ON b.message_id = r.message_id
		WHERE b.body_text LIKE '%café%'
	`).Scan(&n)
	require.NoError(err)
	assert.Equal(2, n, "reactions")

	// Body text must contain the appended [reacted: ...] summary.
	var bodyCount int
	err = st.DB().QueryRow(
		`SELECT COUNT(*) FROM message_bodies WHERE body_text LIKE '%[reacted:%'`,
	).Scan(&bodyCount)
	require.NoError(err)
	assert.GreaterOrEqual(bodyCount, 1, "body with [reacted: suffix")

	err = st.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?", "reacted",
	).Scan(&n)
	require.NoError(err)
	assert.GreaterOrEqual(n, 1, "fts match reacted")
}
