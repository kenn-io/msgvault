//go:build cgo

package sqliteutil

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"io"

	"github.com/mattn/go-sqlite3"
)

// FTS5 first connects by reading its shadow tables while preparing a statement.
// Do that before a caller begins a deferred write transaction: otherwise the
// trigger's first use can establish a read snapshot that another writer makes
// stale before SQLite acquires the write lock (SQLITE_BUSY_SNAPSHOT).
func preloadMetadataFTS(conn *sqlite3.SQLiteConn) error {
	tables, err := openableMetadataFTSTables(conn)
	if err != nil {
		return err
	}
	for _, table := range tables {
		stmt, err := conn.Prepare("SELECT rowid FROM " + table + " LIMIT 0")
		if err != nil {
			return fmt.Errorf("preload %s: %w", table, err)
		}
		if err := stmt.Close(); err != nil {
			return fmt.Errorf("close %s preload: %w", table, err)
		}
	}
	return nil
}

// openableMetadataFTSTables skips indexes this connection cannot load: fresh
// archives have none yet, and builds without FTS5 must still open an archive
// that a capable build indexed.
func openableMetadataFTSTables(conn *sqlite3.SQLiteConn) ([]string, error) {
	rows, err := conn.Query(`SELECT name FROM sqlite_master
		WHERE type = 'table'
		  AND name IN ('messages_metadata_fts', 'participants_metadata_fts', 'recipients_metadata_fts')
		  AND sqlite_compileoption_used('ENABLE_FTS5')`, nil)
	if err != nil {
		return nil, fmt.Errorf("list metadata indexes: %w", err)
	}
	var tables []string
	dest := make([]driver.Value, 1)
	for {
		err = rows.Next(dest)
		if err != nil {
			break
		}
		name, ok := dest[0].(string)
		if !ok {
			err = fmt.Errorf("metadata index name has type %T", dest[0])
			break
		}
		tables = append(tables, name)
	}
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if closeErr := rows.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("close metadata index list: %w", closeErr)
	}
	if err != nil {
		return nil, fmt.Errorf("list metadata indexes: %w", err)
	}
	return tables, nil
}
