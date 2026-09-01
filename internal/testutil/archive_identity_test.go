package testutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFixturesGetDistinctArchiveIdentities pins what cloning must not copy:
// InitSchema() mints one durable archive UID per database, and a fixture
// cloned from a template is a new archive, not the template's twin. Tests that
// hand one archive's cursor or task link to another depend on the difference.
func TestFixturesGetDistinctArchiveIdentities(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	first, err := NewTestStore(t).ArchiveUID()
	require.NoError(err, "first fixture's archive UID")
	second, err := NewTestStore(t).ArchiveUID()
	require.NoError(err, "second fixture's archive UID")

	assert.NotEmpty(first, "fixture carries an archive UID")
	assert.NotEqual(first, second, "each fixture is its own archive")
}
