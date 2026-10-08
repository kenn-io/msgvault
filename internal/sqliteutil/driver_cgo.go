//go:build cgo

package sqliteutil

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/mattn/go-sqlite3"
)

// Available reports whether this build includes the SQLite driver.
const Available = true

var registerDriverOnce sync.Once

// RegisterUnicodeLower adds the deterministic Unicode-aware lowercasing
// function used by metadata search to a SQLite connection.
func RegisterUnicodeLower(conn *sqlite3.SQLiteConn) error {
	if err := conn.RegisterFunc(UnicodeLowerFunction, strings.ToLower, true); err != nil {
		return fmt.Errorf("register %s: %w", UnicodeLowerFunction, err)
	}
	return nil
}

// RegisterFunctions installs the deterministic functions used by Store queries.
// Custom connections used with Store must install the same function set.
func RegisterFunctions(conn *sqlite3.SQLiteConn) error {
	if err := RegisterUnicodeLower(conn); err != nil {
		return err
	}
	if err := conn.RegisterFunc(TimestampKeyFunction, timestampKey, true); err != nil {
		return fmt.Errorf("register %s: %w", TimestampKeyFunction, err)
	}
	return nil
}

// DriverName returns a go-sqlite3 driver whose every connection exposes the
// deterministic functions used by Store queries.
func DriverName() string {
	registerDriverOnce.Do(func() {
		sql.Register(driverName, &sqlite3.SQLiteDriver{
			ConnectHook: RegisterFunctions,
		})
	})
	return driverName
}

// timestampKey returns an exact sortable instant, or SQL NULL for absent,
// invalid and zero timestamps, matching the Store's nullable timestamp reader.
// Separate seconds and nanoseconds retain dates outside UnixNano's range.
func timestampKey(value any) []byte {
	var text string
	switch value := value.(type) {
	case string:
		text = value
	case []byte:
		text = string(value)
	default:
		return nil
	}
	parsed := ParseTime(text)
	if parsed.IsZero() {
		return nil
	}
	return TimestampKey(parsed)
}
