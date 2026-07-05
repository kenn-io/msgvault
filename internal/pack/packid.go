package pack

import (
	"crypto/rand"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

var packIDMu sync.Mutex

// packIDEntropy is monotonic so IDs created within one millisecond still sort
// in creation order. Guarded by packIDMu (ulid.MonotonicEntropy is not
// concurrency-safe).
var packIDEntropy = ulid.Monotonic(rand.Reader, 0)

// NewPackID returns a new lowercase ULID for naming a pack file.
func NewPackID() string {
	packIDMu.Lock()
	defer packIDMu.Unlock()
	return strings.ToLower(ulid.MustNew(ulid.Timestamp(time.Now()), packIDEntropy).String())
}

// IsValidPackID reports whether s is a canonical lowercase ULID.
func IsValidPackID(s string) bool {
	if len(s) != 26 || s != strings.ToLower(s) {
		return false
	}
	_, err := ulid.ParseStrict(strings.ToUpper(s))
	return err == nil
}
