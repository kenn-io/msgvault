package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestInitSchemaWindowHookFiresOnlyForItsOwnStore pins the invariant that makes
// the test-only migration seams safe to use in a binary where more than one
// Store migrates at a time.
//
// The fixtures in internal/testutil build PostgreSQL schemas on background
// workers, so an InitSchema can be running on some other Store at any moment
// while a test has a seam installed. When these seams were package-level
// variables that was a live defect, not a theoretical one: the window hook
// writes through the Store that installed it, so a background migration firing
// it after the installing test returned failed with "sql: database is closed" —
// and, worse, could have written into a live test's archive.
func TestInitSchemaWindowHookFiresOnlyForItsOwnStore(t *testing.T) {
	require := require.New(t)
	owner := testutil.NewTestStore(t)
	other := testutil.NewTestStore(t)

	fired := 0
	restore := owner.SetInitSchemaWindowHookForTest(func() { fired++ })
	defer restore()

	// Another Store's migration must not see this test's hook.
	require.NoError(other.InitSchema())
	require.Zero(fired,
		"a hook installed on one Store fired during another Store's InitSchema: "+
			"concurrent fixture builds can now reach into this test's archive")

	// The hook must still fire for the Store that installed it, or the check
	// above would pass just as well on a seam that never works at all.
	require.NoError(owner.InitSchema())
	require.Equal(1, fired,
		"the hook did not fire on its own Store's InitSchema, so this test proves nothing")

	// And clearing it is scoped the same way.
	restore()
	require.NoError(owner.InitSchema())
	require.Equal(1, fired, "the restore func did not clear the hook")
}
