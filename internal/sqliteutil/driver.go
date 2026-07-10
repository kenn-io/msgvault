// Package sqliteutil provides the SQLite driver variant shared by msgvault's
// production store and tests that exercise SQLite query behavior.
package sqliteutil

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/mattn/go-sqlite3"
)

const (
	driverName           = "msgvault_sqlite3"
	UnicodeLowerFunction = "msgvault_unicode_lower"
)

var registerDriverOnce sync.Once

// DriverName returns a go-sqlite3 driver whose every connection exposes the
// deterministic Unicode-aware lowercasing function used by metadata search.
func DriverName() string {
	registerDriverOnce.Do(func() {
		sql.Register(driverName, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				if err := conn.RegisterFunc(UnicodeLowerFunction, strings.ToLower, true); err != nil {
					return fmt.Errorf("register %s: %w", UnicodeLowerFunction, err)
				}
				return nil
			},
		})
	})
	return driverName
}
