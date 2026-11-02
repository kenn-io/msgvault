package dbtest

import (
	"os"
	"strings"
	"testing"
)

// SkipIfPostgres skips SQLite-only tests when MSGVAULT_TEST_DB targets PostgreSQL.
func SkipIfPostgres(tb testing.TB, reason string) {
	tb.Helper()
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if strings.HasPrefix(testDB, "postgres://") || strings.HasPrefix(testDB, "postgresql://") {
		tb.Skipf("skipping on PostgreSQL: %s", reason)
	}
}
