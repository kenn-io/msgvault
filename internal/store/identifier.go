package store

import (
	"strings"
)

// quoteIdentifier renders a schema-supplied identifier for interpolation into
// SQL: it wraps the name in double quotes and doubles any quote inside it,
// which is how SQL escapes a quote within a quoted identifier and exactly what
// SQLite's own `%w` formatter does.
//
// Identifiers cannot be bound as parameters, so anywhere a column or table name
// reaches SQL it is interpolated — and every one of those names comes from the
// archive, which the user of an archive did not necessarily write. SQLite
// accepts any text as a column name when it is quoted, so
// `CREATE TABLE messages ("evil ON messages BEGIN SELECT 1; END; DROP TABLE
// messages; /*" TEXT)` is a legal table whose column name is a second statement
// waiting for an interpolator, and go-sqlite3 runs every statement in a string
// it is given. Quoting neutralises that; doubling closes the one gap quoting
// alone leaves, a name carrying the quote character itself, which would
// otherwise end the quoting the renderer had just opened.
//
// Escaping rather than refusing such a name, because refusing is a decision
// about the whole archive: the trigger scope this feeds enumerates EVERY live
// column on EVERY open, so one name SQLite considers legal would make the
// archive permanently unopenable — and by the time the refusal lands, earlier
// ALTER TABLE statements in the same initialisation have already committed.
// Every name is renderable, so this cannot fail.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteIdentifiers applies quoteIdentifier to every name.
func quoteIdentifiers(names []string) []string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, quoteIdentifier(name))
	}
	return quoted
}
