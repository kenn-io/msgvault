//go:build cgo

package testutil

/*
// Use SQLite's stable API without requiring system SQLite headers.
extern int sqlite3_config(int, ...);
#define SQLITE_CONFIG_MEMSTATUS 9

static void disable_memstatus(void) {
	sqlite3_config(SQLITE_CONFIG_MEMSTATUS, 0);
}
*/
import "C"

func init() {
	// This test-only setting disables allocation statistics before any connection
	// opens to avoid SQLite's process-wide memory-accounting mutex.
	C.disable_memstatus()
}
