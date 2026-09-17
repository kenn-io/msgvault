//go:build sqlite_vec

// Package vec1 statically registers SQLite's Vec1 vector-search extension.
package vec1

/*
#cgo CFLAGS: -O3 -DNDEBUG -DSQLITE_CORE -DVEC1_STATIC
#cgo linux LDFLAGS: -lm

int sqlite3_vec1_extra_init(const char *);
// sqlite3_vec1_extra_init is provided by the pinned Vec1 amalgamation.
*/
import "C"

const (
	// Version is the pinned upstream Vec1 release version.
	Version = "0.7"

	// MinDimension and MaxDimension bound the vector dimensions accepted by
	// the pinned Vec1 release.
	MinDimension = 2
	MaxDimension = 4096
)

// Auto registers Vec1 for every SQLite connection opened after this call.
// SQLite de-duplicates the extension entrypoint, so repeated calls are safe.
func Auto() {
	C.sqlite3_vec1_extra_init(nil)
}
