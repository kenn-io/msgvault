package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQuoteIdentifierEscapesAnEmbeddedQuote pins the rendering contract every
// interpolated identifier in this package depends on: the quote character is
// doubled, which is how SQL escapes it inside a quoted identifier, so no name
// is unrenderable and no name can close the quoting the renderer opened.
func TestQuoteIdentifierEscapesAnEmbeddedQuote(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{`plain`, `"plain"`},
		{`a column with spaces`, `"a column with spaces"`},
		{`order`, `"order"`},
		{`we"ird`, `"we""ird"`},
		{`""`, `""""""`},
		{`x" END; DROP TABLE messages; --`, `"x"" END; DROP TABLE messages; --"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, quoteIdentifier(tc.name),
				"quoteIdentifier(%q)", tc.name)
		})
	}
}

// TestQuoteIdentifiersEscapesEveryName covers the list form, which is what the
// last_modified trigger's UPDATE OF scope is built from.
func TestQuoteIdentifiersEscapesEveryName(t *testing.T) {
	assert.Equal(t,
		[]string{`"id"`, `"we""ird"`, `"subject"`},
		quoteIdentifiers([]string{`id`, `we"ird`, `subject`}),
		"quoteIdentifiers")
}

// TestQuoteIdentifierRoundTripsThroughSQLite is the half a string comparison
// cannot prove: that SQLite reads the rendered form back as the ORIGINAL name,
// one identifier and not two, and that the payload riding in the name is inert.
func TestQuoteIdentifierRoundTripsThroughSQLite(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	const hostile = `we"ird" ); DROP TABLE t; --`

	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "identifier.db"))
	require.NoError(err, "open db")
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE t (id INTEGER, ` + quoteIdentifier(hostile) + ` TEXT)`)
	require.NoError(err, "create a table whose column name carries a quote and a payload")

	var n int
	require.NoError(db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 't'`).Scan(&n),
		"read sqlite_master")
	require.Equal(1, n, "the payload in the column name must not have executed")

	var got string
	require.NoError(db.QueryRow(
		`SELECT name FROM pragma_table_info('t') WHERE cid = 1`).Scan(&got),
		"read the column name back")
	assert.Equal(hostile, got,
		"SQLite must read the escaped form back as the original name, undoubled")

	_, err = db.Exec(
		`INSERT INTO t (id, ` + quoteIdentifier(hostile) + `) VALUES (1, 'value')`)
	require.NoError(err, "write through the escaped identifier")
	var value string
	require.NoError(db.QueryRow(
		`SELECT `+quoteIdentifier(hostile)+` FROM t WHERE id = 1`).Scan(&value),
		"read through the escaped identifier")
	assert.Equal("value", value, "the escaped identifier must name the same column both ways")
}
