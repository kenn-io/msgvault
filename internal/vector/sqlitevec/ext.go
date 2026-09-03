//go:build sqlite_vec

// Package sqlitevec implements vector.Backend using the sqlite-vec
// SQLite extension, co-located with the main SQLite connection.
//
// Build this package with `-tags sqlite_vec` to enable the extension.
package sqlitevec

import (
	"database/sql"
	"fmt"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	sqlite3 "github.com/mattn/go-sqlite3"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// driverName is the sql.Open driver name exposed by this package.
// It is distinct from the default "sqlite3" registration so callers
// can unambiguously select the vector-enabled variant.
const driverName = "sqlite3_vec"

var registerOnce sync.Once

// RegisterExtension enables the sqlite-vec extension process-wide and
// registers a "sqlite3_vec" driver variant of mattn/go-sqlite3.
//
// sqlite-vec exposes a single entrypoint, [sqlite_vec.Auto], that calls
// sqlite3_auto_extension() to register the extension for every future
// SQLite3 connection in the process. We invoke it here so that both the
// "sqlite3" driver (already registered by mattn/go-sqlite3's init) and
// our "sqlite3_vec" driver observe the extension on every new conn.
//
// The driver variant installs a ConnectHook that runs `PRAGMA
// foreign_keys = ON` on every new connection, because that PRAGMA is
// per-connection in SQLite — applying it once on a pooled *sql.DB
// leaves later connections in the pool with FK enforcement off.
//
// It is safe to call multiple times; the underlying registrations must
// only run once per process, which this function guarantees via sync.Once.
//
// Returns nil today: sqlite_vec.Auto has no error path and sql.Register
// panics rather than returning on duplicate names. The error return is
// preserved for forward compatibility — if either dependency grows a
// fallible registration step, callers won't need to update.
func RegisterExtension() error {
	registerOnce.Do(func() {
		sqlite_vec.Auto()
		sql.Register(driverName, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				if _, err := conn.Exec(`PRAGMA foreign_keys = ON`, nil); err != nil {
					return fmt.Errorf("enable foreign_keys: %w", err)
				}
				return sqliteutil.RegisterUnicodeLower(conn)
			},
		})
	})
	return nil
}

// DriverName returns the driver name to pass to sql.Open.
func DriverName() string { return driverName }

// Available reports whether this build includes sqlite-vec support.
func Available() bool { return true }
