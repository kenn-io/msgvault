package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func TestDuckDBSearchFallbackPipeOR(t *testing.T) {
	engine := NewTestDataBuilder(t).BuildEngine()
	require.Nil(t, engine.sqliteEngine)
	_, err := engine.Search(t.Context(), search.ParseOR("alpha | gamma"), 10, 0)
	assert.ErrorContains(t, err, "pipe OR requires a direct SQLite engine")
}

func TestDuckDBSearchFallbackHiddenTextPresent(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dbPath, db := seedMessageDetailSenderFixture(t)
	_, err := db.Exec(`UPDATE messages SET hidden_text_present = 1 WHERE id = 1`)
	require.NoError(err)
	require.NoError(db.Close())
	engine, err := NewDuckDBEngine("", dbPath, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })
	if !engine.hasSQLite() {
		t.Skip("DuckDB sqlite_scanner extension unavailable")
	}
	require.Nil(engine.sqliteEngine)
	for _, tc := range []struct {
		name, subject string
		hidden        bool
	}{
		{"flagged", "Sender only", true},
		{"unflagged", "Explicit wins", false},
	} {
		rows, err := engine.Search(t.Context(), &search.Query{SubjectTerms: []string{tc.subject}}, 10, 0)
		require.NoError(err, tc.name)
		require.Len(rows, 1, tc.name)
		assert.Equal(tc.hidden, rows[0].HiddenTextPresent, tc.name)
	}
}
