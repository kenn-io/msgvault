//go:build !cgo

package sqliteutil

// Available reports whether this build includes the SQLite driver.
const Available = false

// DriverName returns the unregistered name; database/sql rejects SQLite opens.
func DriverName() string { return driverName }
