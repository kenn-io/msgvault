// Package sqliteutil provides the SQLite driver variant shared by msgvault's
// production store and tests that exercise SQLite query behavior. Builds
// without CGO include no SQLite driver; Available reports which one this is.
package sqliteutil

const (
	driverName           = "msgvault_sqlite3"
	UnicodeLowerFunction = "msgvault_unicode_lower"
)
