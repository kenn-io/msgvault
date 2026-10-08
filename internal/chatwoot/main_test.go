package chatwoot

import (
	"os"
	"testing"
	"time"
)

// Fixtures date from early 2026; a fixed clock keeps their recent media and
// calls inside the refresh window regardless of when the tests run.
func TestMain(m *testing.M) {
	now = func() time.Time { return time.Unix(1767229200, 0).UTC() }
	os.Exit(m.Run())
}
