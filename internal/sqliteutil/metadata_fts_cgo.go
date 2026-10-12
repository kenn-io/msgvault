//go:build cgo

package sqliteutil

import (
	"fmt"
	"strings"

	"github.com/mattn/go-sqlite3"
)

// FTS5 first connects by reading its shadow tables while preparing a statement.
// Do that before a caller begins a deferred write transaction: otherwise the
// trigger's first use can establish a read snapshot that another writer makes
// stale before SQLite acquires the write lock (SQLITE_BUSY_SNAPSHOT).
func preloadMetadataFTS(conn *sqlite3.SQLiteConn) error {
	for _, table := range []string{"messages_metadata_fts", "participants_metadata_fts", "recipients_metadata_fts"} {
		stmt, err := conn.Prepare("SELECT rowid FROM " + table + " LIMIT 0")
		if err != nil {
			// Fresh archives and builds without FTS5 must still open for setup.
			if strings.Contains(err.Error(), "no such table: "+table) || strings.Contains(err.Error(), "no such module: fts5") {
				continue
			}
			return fmt.Errorf("preload %s: %w", table, err)
		}
		if err := stmt.Close(); err != nil {
			return fmt.Errorf("close %s preload: %w", table, err)
		}
	}
	return nil
}
