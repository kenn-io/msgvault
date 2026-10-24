//go:build cgo

package testutil

/*
// Use SQLite's stable API without requiring system SQLite headers.
extern int sqlite3_config(int, ...);
#define SQLITE_CONFIG_MEMSTATUS 9

static int disable_memstatus(void) {
	return sqlite3_config(SQLITE_CONFIG_MEMSTATUS, 0);
}
*/
import "C"

import "fmt"

func init() {
	// This test-only setting disables allocation statistics before any connection
	// opens to avoid SQLite's process-wide memory-accounting mutex.
	if rc := C.disable_memstatus(); rc != 0 {
		panic(fmt.Sprintf("testutil: disabling SQLite memory accounting failed with code %d; SQLite was already initialized", int(rc)))
	}
}
