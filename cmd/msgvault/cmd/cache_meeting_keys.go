package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
	"go.kenn.io/msgvault/internal/store"
)

var sourceTimePattern = regexp.MustCompile(`(?i)\b(scheduled_at|original_start_at|start_at|end_at|recurrence_end_at|created_at)\b`)

var _ store.SQLReader = (*cacheSourceSnapshot)(nil)

func (s *cacheSourceSnapshot) sourceQuery(query string, args []any) string {
	statement := fmt.Sprintf("SELECT * FROM sqlite_query('sqlite_db', '%s'", strings.ReplaceAll(query, "'", "''"))
	if len(args) > 0 {
		statement += ", params=row(" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
	}
	statement += ")"
	// sqlite_query reports SQLite DATETIME projections as strings. The Store
	// calendar/key readers scan these named projections into sql.NullTime.
	// Convert only selected timestamp fields, preserving NULL and UTC instants;
	// eligibility predicates mentioning a time do not add a result column.
	// A nested FROM before the outer projection boundary disables rewriting;
	// only flat Store projections are cast.
	var replacements []string
	seen := make(map[string]bool)
	if projection, flat := sourceTimestampProjection(query); flat {
		for _, field := range sourceTimePattern.FindAllString(projection, -1) {
			field = strings.ToLower(field)
			if seen[field] {
				continue
			}
			seen[field] = true
			replacements = append(replacements, fmt.Sprintf("(CAST(%s AS TIMESTAMPTZ) AT TIME ZONE 'UTC') AS %s", field, field))
		}
	}
	if len(replacements) > 0 {
		statement = "SELECT * REPLACE (" + strings.Join(replacements, ", ") + ") FROM (" + statement + ")"
	}
	return statement
}

func sourceTimestampProjection(query string) (string, bool) {
	depth := 0
	nestedFrom := false
	quote := byte(0)
	lineComment := false
	blockComment := false
	for i := 0; i < len(query); {
		current := query[i]
		if lineComment {
			if current == '\n' {
				lineComment = false
			}
			i++
			continue
		}
		if blockComment {
			if current == '*' && i+1 < len(query) && query[i+1] == '/' {
				blockComment = false
				i += 2
			} else {
				i++
			}
			continue
		}
		if quote != 0 {
			closing := quote
			if quote == '[' {
				closing = ']'
			}
			if current == closing {
				if quote != '[' && i+1 < len(query) && query[i+1] == closing {
					i += 2
					continue
				}
				quote = 0
			}
			i++
			continue
		}
		if current == '-' && i+1 < len(query) && query[i+1] == '-' {
			lineComment = true
			i += 2
			continue
		}
		if current == '/' && i+1 < len(query) && query[i+1] == '*' {
			blockComment = true
			i += 2
			continue
		}
		switch current {
		case '\'', '"', '`', '[':
			quote = current
			i++
		case '(':
			depth++
			i++
		case ')':
			if depth > 0 {
				depth--
			}
			i++
		default:
			if i+4 <= len(query) && strings.EqualFold(query[i:i+4], "FROM") &&
				(i == 0 || !sourceSQLWordByte(query[i-1])) &&
				(i+4 == len(query) || !sourceSQLWordByte(query[i+4])) {
				if depth == 0 {
					if nestedFrom {
						return "", false
					}
					return query[:i], true
				}
				nestedFrom = true
				i += 4
				continue
			}
			i++
		}
	}
	return "", false
}

func sourceSQLWordByte(value byte) bool {
	return value == '_' || value == '$' ||
		(value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
		(value >= '0' && value <= '9')
}

func (s *cacheSourceSnapshot) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if s.sqliteTx != nil {
		return s.sqliteTx.QueryContext(ctx, query, args...)
	}
	return s.duckTx.QueryContext(ctx, s.sourceQuery(query, args), sqliteQueryArgs(args)...)
}
func (s *cacheSourceSnapshot) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if s.sqliteTx != nil {
		return s.sqliteTx.QueryRowContext(ctx, query, args...)
	}
	return s.duckTx.QueryRowContext(ctx, s.sourceQuery(query, args), sqliteQueryArgs(args)...)
}

func sqliteQueryArgs(args []any) []any {
	converted := make([]any, len(args))
	for i, arg := range args {
		if timestamp, ok := arg.(time.Time); ok {
			// sqlite_query binds params to SQLite and does not accept DuckDB's
			// TIMESTAMP WITH TIME ZONE value. Match the archive driver's text
			// encoding so DATETIME predicates keep their native comparison.
			converted[i] = timestamp.Format(sqlite3.SQLiteTimestampFormats[0])
			continue
		}
		converted[i] = arg
	}
	return converted
}

// Resolve keys before CSV preparation closes the source transaction. All
// source reads use that transaction; DuckDB only receives the resolved values.
func stageCacheMeetingKeys(ctx context.Context, snapshot *cacheSourceSnapshot, dbPath string, after, through int64) error {
	db := snapshot.DuckDB()
	// The CSV path returns a pooled database; regular tables keep staged keys
	// visible to every export connection in the build-local DuckDB.
	if _, err := db.ExecContext(ctx, `CREATE OR REPLACE TABLE cache_meeting_keys(message_id BIGINT PRIMARY KEY, meeting_key VARCHAR)`); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	haveCursor := after > 0
	for {
		statement := `SELECT id FROM messages WHERE message_type='meeting_transcript' AND ` + exportableMessageWhere("") + ` AND id <= ?`
		args := []any{through}
		if haveCursor {
			statement += ` AND id > ?`
			args = append(args, after)
		}
		statement += ` ORDER BY id LIMIT 100`
		ids, err := cacheTranscriptIDBatch(ctx, snapshot, statement, args)
		if err != nil {
			return fmt.Errorf("enumerate transcript keys: %w", err)
		}
		if len(ids) == 0 {
			return ctx.Err()
		}
		keys, err := st.ExportLogicalMeetingKeysContext(ctx, snapshot, ids)
		if err != nil {
			return err
		}
		rows := make([][]any, 0, len(ids))
		for _, id := range ids {
			key, ok := keys[id]
			if !ok {
				return fmt.Errorf("transcript %d absent from key snapshot", id)
			}
			rows = append(rows, []any{id, key})
		}
		if err := insertCacheRows(ctx, db, "cache_meeting_keys", rows); err != nil {
			return fmt.Errorf("stage meeting keys: %w", err)
		}
		after = ids[len(ids)-1]
		haveCursor = true
	}
}

// Close the enumeration before resolving keys through the same pinned reader.
func cacheTranscriptIDBatch(ctx context.Context, snapshot *cacheSourceSnapshot, statement string, args []any) (ids []int64, err error) {
	rows, err := snapshot.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func snapshotDerivedRevision(snapshot *cacheSourceSnapshot) (int64, error) {
	var revision int64
	err := snapshot.QueryRow(`SELECT coalesce((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key='derived_data_revision'),0)`).Scan(&revision)
	return revision, err
}

func snapshotRevisionsRelatedOnly(snapshot *cacheSourceSnapshot, previous, current int64) (bool, error) {
	if current == previous {
		return true, nil
	}
	var count int64
	err := snapshot.QueryRow(`SELECT count(*) FROM cache_related_revision_journal WHERE revision > ? AND revision <= ?`, previous, current).Scan(&count)
	return count == current-previous, err
}
